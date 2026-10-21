package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silo-Server/silo-server/internal/requestlock"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/stream"
)

var ErrInvalidVirtualMedia = errors.New("invalid virtual media")

// ErrVirtualReconcileRefused reports that a destructive reconciliation was
// rejected because the caller did not carry positive completeness evidence
// for the source's membership, or because the keep set shrank beyond what a
// complete enumeration can plausibly explain. It is distinct from a transient
// failure: retrying without new evidence is refused again.
var ErrVirtualReconcileRefused = errors.New("virtual reconciliation refused")

const (
	maxVirtualURIBytes             = 1024
	maxVirtualQueryValueBytes      = 256
	maxVirtualSourceBytes          = 128
	maxVirtualTitleBytes           = 512
	maxVirtualOverviewBytes        = 32 << 10
	maxVirtualArtworkPathBytes     = 2048
	maxVirtualVariantLabelBytes    = 256
	maxVirtualAttributeBytes       = 128
	maxVirtualIdentifierBytes      = 128
	maxVirtualGenres               = 64
	maxVirtualGenreBytes           = 128
	maxVirtualEpisodes             = 4096
	maxVirtualVariantsPerMedia     = 50
	maxVirtualFilesPerRegistration = 4096
	maxVirtualLanguages            = 32
	maxVirtualLanguageBytes        = 35

	// reconcileImplausibleRemovalFloor is the smallest absolute number of
	// existing source claims whose removal can be judged implausible during a
	// verified reconciliation. Below it the keep ratio is too noisy to tell a
	// genuine provider removal from a truncated keep set, so a small, deliberate
	// withdrawal is allowed. At or above it, dropping more than half of the
	// claims is treated as evidence the enumeration was incomplete.
	reconcileImplausibleRemovalFloor = 5
)

// virtualItemRetentionGuard is a SQL predicate fragment, correlated to the
// candidate media_items alias `mi`, that stops a background virtual-catalog
// sweep from deleting an item that still has user-visible state. Watch history
// and watch progress reference content by bare ID with no foreign key, so a
// delete silently strands Continue Watching/history rows the client then 404s
// and refetches in a loop. Open abs sessions and unexpired protocol-v3 attempts
// are live playback state. Embed the fragment inside the DELETE so the check and
// the delete are one statement, rather than a separate read that a concurrent
// insert could overtake. The last_file_id correlation only sees surviving files,
// so the file sweep must carry the matching reference in
// virtualFileRetentionGuard.
const virtualItemRetentionGuard = `
	  AND NOT EXISTS (SELECT 1 FROM user_watch_history uwh WHERE uwh.media_item_id = mi.content_id)
	  AND NOT EXISTS (
	      SELECT 1 FROM user_watch_progress uwp
	      WHERE uwp.media_item_id = mi.content_id
	         OR uwp.last_file_id IN (SELECT mf.id FROM media_files mf WHERE mf.content_id = mi.content_id)
	  )
	  AND NOT EXISTS (
	      SELECT 1 FROM abs_playback_sessions aps
	      WHERE aps.content_id = mi.content_id AND aps.closed_at IS NULL
	  )
	  AND NOT EXISTS (
	      SELECT 1 FROM playback_v3_attempts pva
	      WHERE pva.expires_at > NOW()
	        AND (pva.effective_media_file_id IN (SELECT mf.id FROM media_files mf WHERE mf.content_id = mi.content_id)
	          OR pva.requested_media_file_id IN (SELECT mf.id FROM media_files mf WHERE mf.content_id = mi.content_id))
	  )`

// virtualFileRetentionGuard is a SQL predicate fragment, correlated to the
// candidate media_files alias `mf`, that stops a virtual file delete from
// destroying live playback or the user's last-played version. playback_v3_attempts
// has ON DELETE CASCADE foreign keys to media_files, so without this a file
// sweep would destroy the live attempt before the item-level guard could observe
// it. An open abs_playback_sessions row references its file with ON DELETE SET
// NULL, so deleting the row silently strips a live session's file identity. A
// stale candidate that is still user_watch_progress.last_file_id is retained so
// a known-working version does not vanish when the provider re-lists with new
// result ids — the same retention the scanner applies in
// ReplaceVirtualCandidates. This is what makes the last_file_id case in
// virtualItemRetentionGuard effective: the file sweep runs before the item
// delete, so without the file-level guard the item-level subquery would already
// be looking at a deleted row. Embedding it in the DELETE keeps the check and
// the delete in one statement so a concurrent insert cannot race it.
const virtualFileRetentionGuard = `
	  AND NOT EXISTS (
	      SELECT 1 FROM user_watch_progress uwp WHERE uwp.last_file_id = mf.id
	  )
	  AND NOT EXISTS (
	      SELECT 1 FROM abs_playback_sessions aps
	      WHERE aps.media_file_id = mf.id AND aps.closed_at IS NULL
	  )
	  AND NOT EXISTS (
	      SELECT 1 FROM playback_v3_attempts pva
	      WHERE pva.expires_at > NOW()
	        AND (pva.effective_media_file_id = mf.id OR pva.requested_media_file_id = mf.id)
	  )`

var (
	virtualPathSegmentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	imdbIDPattern             = regexp.MustCompile(`^tt[0-9]{1,16}$`)
	numericProviderIDPattern  = regexp.MustCompile(`^[1-9][0-9]{0,19}$`)
)

type VirtualMediaVariant struct {
	VirtualURI        string
	Label             string
	Resolution        string
	CodecVideo        string
	CodecAudio        string
	HDR               string
	Bitrate           int
	RuntimeMinutes    int
	FileSize          int64
	Container         string
	SourceType        string
	AudioLanguages    []string
	SubtitleLanguages []string
	Availability      string
}

type VirtualEpisode struct {
	SeasonNumber, EpisodeNumber            int
	Title, Overview, StillPath, VirtualURI string
	AirDate                                time.Time
	RuntimeMinutes                         int
	Resolution                             string
	CodecVideo                             string
	CodecAudio                             string
	HDR                                    string
	Bitrate                                int
	FileSize                               int64
	Container                              string
	SourceType                             string
	AudioLanguages                         []string
	SubtitleLanguages                      []string
	Variants                               []VirtualMediaVariant
}

type VirtualMedia struct {
	LibraryID, MediaType, Title          string
	Year                                 int
	IMDbID, TMDBID, TVDBID, Overview     string
	Genres                               []string
	PosterPath, BackdropPath, VirtualURI string
	RuntimeMinutes                       int
	Source                               string
	Resolution                           string
	CodecVideo                           string
	CodecAudio                           string
	HDR                                  string
	Bitrate                              int
	FileSize                             int64
	Container                            string
	SourceType                           string
	AudioLanguages                       []string
	SubtitleLanguages                    []string
	Episodes                             []VirtualEpisode
	Variants                             []VirtualMediaVariant
}

type VirtualMediaResult struct {
	MediaID          string
	LibraryID        string
	EpisodesUpserted int
}

type VirtualReconcileResult struct {
	ItemsRemoved int
	FilesRemoved int
}

// VirtualReconcileEvidence attests that a keep set was produced by a complete
// enumeration of a source's membership. The zero value is not complete and
// cannot authorize destructive reconciliation through the verified entry
// point. FullCycle is set only by a pass that walked the caller's entire queue
// from the front without exhausting its budget; a resumed or deadline-cut
// pass leaves it false. SourceCount is the number of queue items the caller
// enumerated for the source, and QueueCount is the size of the whole snapshot
// the keep set was drawn from.
type VirtualReconcileEvidence struct {
	FullCycle   bool
	SourceCount int
	QueueCount  int
}

// VirtualMediaRegistrar owns transactional catalog registration for virtual
// sources. Plugins submit stable external identifiers and URIs; they never
// receive database access or depend on Silo's table layout.
type VirtualMediaRegistrar struct {
	pool                *pgxpool.Pool
	TMDBDigitalReleases TMDBDigitalReleaseChecker
	ReleaseOverrides    ReleaseOverrideLookup
	EpisodeReleaseDates interface {
		EpisodeReleaseDates(context.Context, int, int) (map[int]time.Time, error)
	}
}

func NewVirtualMediaRegistrar(pool *pgxpool.Pool) *VirtualMediaRegistrar {
	return &VirtualMediaRegistrar{pool: pool, ReleaseOverrides: NewReleaseOverrideRepository(pool)}
}

// Pool exposes the registrar's database pool to core services that need to
// coordinate work with the same database session without taking ownership of
// the pool.
func (r *VirtualMediaRegistrar) Pool() *pgxpool.Pool {
	if r == nil {
		return nil
	}
	return r.pool
}

func (r *VirtualMediaRegistrar) Upsert(ctx context.Context, in VirtualMedia) (*VirtualMediaResult, error) {
	return r.UpsertVirtualMedia(ctx, 0, in)
}

// UpsertVirtualMedia is the plugin-host entry point. The installation ID is
// persisted as ownership metadata so reconciliation cannot remove another
// plugin's virtual catalog entries.
func (r *VirtualMediaRegistrar) UpsertVirtualMedia(ctx context.Context, installationID int, in VirtualMedia) (*VirtualMediaResult, error) {
	if r == nil || r.pool == nil {
		return nil, errors.New("virtual catalog is unavailable")
	}
	if in.MediaType == "series" {
		in = r.normalizeSeriesVirtualMedia(ctx, in)
		in = r.fillEpisodeReleaseDates(ctx, in)
	}
	if err := validateVirtualMedia(in); err != nil {
		return nil, err
	}
	var releaseSnapshot []ReleaseOverride
	eligible := false
	permitViaPhysical := false
	movieIDs := releaseIdentities("movie", in.TMDBID, "", in.IMDbID, 0, 0)
	if in.MediaType == "movie" {
		// Resolve the full alias set before deciding whether provider
		// evidence is needed: a stored past override permits without any
		// provider call, and a stored future override blocks. Skipped only
		// when override reads are unwired (then no stored aliases can be
		// consulted and behavior matches the pre-override gate).
		if r.ReleaseOverrides != nil {
			var err error
			movieIDs, err = releaseIdentitiesForContent(ctx, r.pool, "movie", virtualContentID(in), "movie", in.TMDBID, "", in.IMDbID, 0, 0)
			if err != nil {
				return nil, err
			}
		}
		// Single decision from the captured entries at one evaluation
		// time; the transaction-time revalidation below re-decides from
		// its own validated snapshot.
		now := time.Now().UTC()
		var err error
		releaseSnapshot, err = captureReleaseOverrides(ctx, r.ReleaseOverrides, movieIDs)
		if err != nil {
			return nil, err
		}
		allowed, active := decideReleaseOverrides(now, releaseSnapshot)
		switch {
		case active:
			eligible = allowed
		default:
			// Possession proves release even when provider data is
			// missing, negative, or dated in the future. Requires
			// override wiring so the unwired fail-fast path stays
			// database-free.
			if r.ReleaseOverrides != nil {
				physical, err := hasPhysicalMediaFiles(ctx, r.pool, virtualContentID(in))
				if err != nil {
					return nil, err
				}
				if physical {
					eligible, permitViaPhysical = true, true
					break
				}
			}
			if isFutureDate(in.Year, "") {
				eligible = false
			} else {
				tmdbID, _ := strconv.Atoi(strings.TrimSpace(in.TMDBID))
				eligible, err = newTheatricalReleaseGate(r.TMDBDigitalReleases).lookupProvider(ctx, tmdbID)
				if err != nil {
					return nil, err
				}
			}
		}
	}
	folderID, _ := strconv.Atoi(in.LibraryID)
	source := normalizedVirtualSource(in.Source)
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin virtual media transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Shared purge barrier: allows concurrent writers but excludes them
	// during the purge sweep. Taken before the content and installation
	// locks below so both sides order the shared lock first and the
	// AB-BA cycle cannot form.
	if err := requestlock.LockPurgeBarrierShared(ctx, tx); err != nil {
		return nil, fmt.Errorf("lock purge barrier: %w", err)
	}
	// Exclusive content lock first (see lockReleaseContentTx): registration
	// that owns item metadata can insert or change scalar aliases, so it is
	// an alias writer. No shared-to-exclusive upgrade happens later.
	if in.MediaType == "movie" || in.MediaType == "series" {
		if err := lockReleaseContentTx(ctx, tx, virtualContentID(in), true); err != nil {
			return nil, fmt.Errorf("lock release content: %w", err)
		}
	}
	if err := lockVirtualMediaInstallation(ctx, tx, installationID); err != nil {
		return nil, err
	}
	if err := lockVirtualMediaSource(ctx, tx, installationID, source); err != nil {
		return nil, err
	}
	var folderType string
	if err := tx.QueryRow(ctx, `SELECT type FROM media_folders WHERE id=$1 AND enabled IS NOT FALSE`, folderID).Scan(&folderType); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%w: destination library does not exist or is disabled", ErrInvalidVirtualMedia)
		}
		return nil, fmt.Errorf("load destination library: %w", err)
	}
	if !virtualLibraryCompatible(folderType, in.MediaType) {
		return nil, fmt.Errorf("%w: %s cannot be added to %s library", ErrInvalidVirtualMedia, in.MediaType, folderType)
	}
	if err := validateReleaseSnapshotTx(ctx, tx, releaseSnapshot); err != nil {
		return nil, err
	}
	contentID := virtualContentID(in)
	ownsItemMetadata, err := upsertVirtualMediaItem(ctx, tx, installationID, source, contentID, in)
	if err != nil {
		return nil, err
	}
	if in.MediaType == "movie" && r.ReleaseOverrides != nil {
		// The item row is locked and the exclusive content lock (taken at
		// transaction start) holds alias writers out, so this is the
		// authoritative alias set. The evaluated snapshot must be a subset of
		// the fresh identities: removed aliases lose authority, while new
		// aliases from registrar self-correction or metadata enrichment are
		// accepted as long as every snapshot alias survives.
		freshIDs, err := releaseIdentitiesForContent(ctx, tx, "movie", contentID, "movie", in.TMDBID, "", in.IMDbID, 0, 0)
		if err != nil {
			return nil, err
		}
		if len(releaseSnapshot) > 0 {
			if !releaseIdentitiesCovered(snapshotIdentities(releaseSnapshot), freshIDs) {
				return nil, fmt.Errorf("%w: release alias set changed during registration", ErrReleaseOverrideConflict)
			}
		}
		freshEntries, err := captureReleaseOverridesTx(ctx, tx, freshIDs)
		if err != nil {
			return nil, err
		}
		if err := validateReleaseSnapshotTx(ctx, tx, freshEntries); err != nil {
			return nil, err
		}
		if allowed, active := decideReleaseOverrides(time.Now().UTC(), freshEntries); active {
			eligible = allowed
		} else if permitViaPhysical {
			// Re-verify possession inside the transaction: files are
			// managed outside this lock protocol.
			physical, err := hasPhysicalMediaFiles(ctx, tx, contentID)
			if err != nil {
				return nil, err
			}
			eligible = physical
		}
		// Otherwise eligible retains the pre-transaction provider verdict:
		// the exclusive content lock prevents alias changes, so the
		// pre-transaction lookup remains valid and must not be repeated
		// while holding locks (provider latency would block all alias
		// writers).
		movieIDs = freshIDs
	}
	if _, err := tx.Exec(ctx, `INSERT INTO media_item_libraries(content_id,media_folder_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, contentID, folderID); err != nil {
		return nil, fmt.Errorf("link virtual media: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO virtual_media_source_claims(
			plugin_installation_id, source_key, content_id, media_folder_id, owns_item_metadata, last_seen_at
		) VALUES($1,$2,$3,$4,$5,NOW())
		ON CONFLICT(plugin_installation_id,source_key,content_id,media_folder_id) DO UPDATE SET
			owns_item_metadata=EXCLUDED.owns_item_metadata,
			last_seen_at=NOW(),
			updated_at=NOW()`, installationID, source, contentID, folderID, ownsItemMetadata); err != nil {
		return nil, fmt.Errorf("claim virtual media source: %w", err)
	}

	if in.MediaType == "movie" {
		reason := ""
		if !eligible {
			reason = "no_home_release"
		}
		if err := recordReleaseMetadata(ctx, tx, movieIDs, reason); err != nil {
			return nil, err
		}
	}
	filePaths := make([]string, 0, virtualMediaFileCount(in))
	episodes := 0
	if in.MediaType == "series" {
		// Resolve the series alias template once: incoming IDs unioned
		// with stored scalars and provider-table rows. Incoming aliases
		// gate every episode even when metadata ownership prevents
		// persisting them.
		seriesTemplate, err := releaseIdentitiesForContent(ctx, tx, "episode", contentID, "series", in.TMDBID, in.TVDBID, in.IMDbID, 1, 1)
		if err != nil {
			return nil, err
		}
		for _, ep := range in.Episodes {
			playable, epPaths, err := upsertVirtualEpisode(ctx, tx, contentID, folderID, installationID, ownsItemMetadata, seriesTemplate, ep)
			if err != nil {
				return nil, err
			}
			if playable {
				filePaths = append(filePaths, epPaths...)
				episodes++
			}
		}
	} else {
		if eligible {
			if len(in.Variants) > 0 {
				for _, v := range in.Variants {
					if err := upsertVirtualFileVariant(ctx, tx, contentID, "", folderID, installationID, v, in.RuntimeMinutes); err != nil {
						return nil, err
					}
					filePaths = append(filePaths, v.VirtualURI)
				}
			} else if in.VirtualURI != "" {
				if err := upsertVirtualFileWithMeta(ctx, tx, contentID, "", folderID, installationID, in.VirtualURI, runtimeSeconds(in.RuntimeMinutes), in); err != nil {
					return nil, err
				}
				filePaths = append(filePaths, in.VirtualURI)
			}
		}
	}
	if err := syncVirtualFileSourceClaims(ctx, tx, installationID, source, contentID, folderID, filePaths); err != nil {
		return nil, err
	}
	if err := EnqueueSearchIndexUpsert(ctx, tx, contentID); err != nil {
		return nil, fmt.Errorf("enqueue virtual media search update: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit virtual media transaction: %w", err)
	}
	return &VirtualMediaResult{MediaID: contentID, LibraryID: in.LibraryID, EpisodesUpserted: episodes}, nil
}

func upsertVirtualMediaItem(ctx context.Context, tx pgx.Tx, installationID int, source, contentID string, in VirtualMedia) (bool, error) {
	if err := requestlock.LockItem(ctx, tx, contentID); err != nil {
		return false, fmt.Errorf("lock virtual media identity: %w", err)
	}
	status := "unmatched"
	if in.Overview != "" || in.PosterPath != "" {
		status = "matched"
	}
	var insertedID string
	err := tx.QueryRow(ctx, `
		INSERT INTO media_items(
			content_id,type,title,year,imdb_id,tmdb_id,tvdb_id,overview,genres,poster_path,backdrop_path,
			runtime,matched_at,status,virtual_owner_installation_id,virtual_source,virtual_last_seen_at
		) VALUES(
			$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,
			CASE WHEN $13='matched' THEN NOW() END,$13,NULLIF($14,0),$15,NOW()
		)
		ON CONFLICT(content_id) DO NOTHING
		RETURNING content_id`,
		contentID, in.MediaType, in.Title, in.Year, in.IMDbID, in.TMDBID, in.TVDBID,
		in.Overview, in.Genres, in.PosterPath, in.BackdropPath, in.RuntimeMinutes, status, installationID, source,
	).Scan(&insertedID)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("insert virtual media: %w", err)
	}

	var (
		existingType       string
		existingOwner      *int64
		existingSource     string
		hasOwnClaim        bool
		hasOtherOwnerClaim bool
		hasPhysicalFile    bool
	)
	if err := tx.QueryRow(ctx, `
		SELECT mi.type, mi.virtual_owner_installation_id, mi.virtual_source,
			EXISTS(
				SELECT 1 FROM virtual_media_source_claims vmsc
				WHERE vmsc.content_id=mi.content_id
				  AND vmsc.plugin_installation_id=$2
				  AND vmsc.source_key=$3
			),
			EXISTS(
				SELECT 1 FROM virtual_media_source_claims vmsc
				WHERE vmsc.content_id=mi.content_id
				  AND vmsc.owns_item_metadata
				  AND (vmsc.plugin_installation_id<>$2 OR vmsc.source_key<>$3)
			),
			EXISTS(
				SELECT 1 FROM media_files mf
				WHERE mf.content_id=mi.content_id
				  AND mf.container<>'virtual'
				  AND mf.file_path NOT LIKE 'virtual://%'
			)
		FROM media_items mi
		WHERE mi.content_id=$1
		FOR UPDATE`, contentID, installationID, source).Scan(
		&existingType, &existingOwner, &existingSource, &hasOwnClaim, &hasOtherOwnerClaim, &hasPhysicalFile,
	); err != nil {
		return false, fmt.Errorf("load existing virtual media identity: %w", err)
	}
	if existingType != in.MediaType {
		return false, fmt.Errorf("%w: canonical content ID already belongs to media type %q", ErrInvalidVirtualMedia, existingType)
	}

	if hasPhysicalFile {
		if _, err := tx.Exec(ctx, `
			UPDATE virtual_media_source_claims
			SET owns_item_metadata=false,updated_at=NOW()
			WHERE content_id=$1 AND owns_item_metadata`, contentID); err != nil {
			return false, fmt.Errorf("release virtual metadata ownership to local media: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE media_items
			SET virtual_owner_installation_id=NULL,virtual_source='',virtual_last_seen_at=NULL,updated_at=NOW()
			WHERE content_id=$1`, contentID); err != nil {
			return false, fmt.Errorf("clear virtual metadata ownership from local media: %w", err)
		}
		return false, nil
	}

	currentScalarOwner := existingOwner != nil && int(*existingOwner) == installationID && existingSource == source
	if installationID == 0 {
		currentScalarOwner = existingOwner == nil && existingSource == source && hasOwnClaim
	}
	ownsItemMetadata := currentScalarOwner || (existingOwner == nil && !hasOtherOwnerClaim)
	if !ownsItemMetadata {
		// Existing local items and items registered by another plugin remain the
		// authoritative catalog record. This registration owns only its virtual
		// source association and files, preserving local+virtual coexistence.
		return false, nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE media_items SET
			title=$2,
			year=CASE WHEN $3>0 THEN $3 ELSE year END,
			imdb_id=CASE WHEN $4<>'' THEN $4 ELSE imdb_id END,
			tmdb_id=CASE WHEN $5<>'' THEN $5 ELSE tmdb_id END,
			tvdb_id=CASE WHEN $6<>'' THEN $6 ELSE tvdb_id END,
			overview=CASE WHEN $7<>'' THEN $7 ELSE overview END,
			genres=CASE WHEN cardinality($8::text[])>0 THEN $8 ELSE genres END,
			poster_path=CASE WHEN $9<>'' THEN $9 ELSE poster_path END,
			backdrop_path=CASE WHEN $10<>'' THEN $10 ELSE backdrop_path END,
			runtime=CASE WHEN $11>0 THEN $11 ELSE runtime END,
			matched_at=CASE WHEN $12='matched' THEN COALESCE(matched_at,NOW()) ELSE matched_at END,
			status=CASE WHEN $12='matched' THEN 'matched' ELSE status END,
			virtual_owner_installation_id=NULLIF($13,0),
			virtual_source=$14,
			virtual_last_seen_at=NOW(),
			updated_at=NOW()
		WHERE content_id=$1`,
		contentID, in.Title, in.Year, in.IMDbID, in.TMDBID, in.TVDBID, in.Overview,
		in.Genres, in.PosterPath, in.BackdropPath, in.RuntimeMinutes, status, installationID, source,
	); err != nil {
		return false, fmt.Errorf("update owned virtual media: %w", err)
	}
	return true, nil
}

func virtualMediaFileCount(in VirtualMedia) int {
	if in.MediaType == "movie" {
		if len(in.Variants) > 0 {
			return len(in.Variants)
		}
		return 1
	}
	total := 0
	for _, episode := range in.Episodes {
		if len(episode.Variants) > 0 {
			total += len(episode.Variants)
		} else if episode.VirtualURI != "" {
			total++
		}
	}
	return total
}

func syncVirtualFileSourceClaims(ctx context.Context, tx pgx.Tx, installationID int, source, contentID string, folderID int, filePaths []string) error {
	if filePaths == nil {
		filePaths = []string{}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO virtual_media_file_source_claims(
			plugin_installation_id,source_key,content_id,media_folder_id,file_path,last_seen_at
		)
		SELECT $1,$2,$3,$4,path,NOW()
		FROM unnest($5::text[]) AS path
		ON CONFLICT(plugin_installation_id,source_key,content_id,media_folder_id,file_path) DO UPDATE SET
			last_seen_at=NOW(),
			updated_at=NOW()`, installationID, source, contentID, folderID, filePaths); err != nil {
		return fmt.Errorf("claim virtual media files: %w", err)
	}
	rows, err := tx.Query(ctx, `
		DELETE FROM virtual_media_file_source_claims
		WHERE plugin_installation_id=$1
		  AND source_key=$2
		  AND content_id=$3
		  AND media_folder_id=$4
		  AND NOT (file_path=ANY($5::text[]))
		RETURNING file_path`, installationID, source, contentID, folderID, filePaths)
	if err != nil {
		return fmt.Errorf("delete stale virtual file claims: %w", err)
	}
	deletedPaths, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("collect stale virtual file paths: %w", err)
	}
	if len(deletedPaths) > 0 {
		if _, err := tx.Exec(ctx, `
			DELETE FROM media_files mf
			WHERE mf.content_id=$1
			  AND mf.media_folder_id=$2
			  AND mf.file_path=ANY($3::text[])
			  AND mf.virtual_owner_installation_id=$4
			  AND (mf.container='virtual' OR mf.file_path LIKE 'virtual://%')
			  AND NOT EXISTS(
				SELECT 1 FROM virtual_media_file_source_claims vmfsc
				WHERE vmfsc.plugin_installation_id=$4
				  AND vmfsc.content_id=$1
				  AND vmfsc.media_folder_id=$2
				  AND vmfsc.file_path=mf.file_path
			  )`+virtualFileRetentionGuard, contentID, folderID, deletedPaths, installationID); err != nil {
			return fmt.Errorf("remove stale virtual media files: %w", err)
		}
	}
	return nil
}

// ReconcileVirtualMedia removes stale virtual media owned by one source.
// Physical files and collection-linked items are preserved. Core-owned media
// uses installationID 0; plugin-owned media uses a positive installation ID.
//
// This is the explicit-caller entry point: a caller that supplies keepIDs is
// asserting its own list. The core monitor uses ReconcileVirtualMediaVerified
// so destructive reconciliation requires a full-cycle completeness attestation.
func (r *VirtualMediaRegistrar) ReconcileVirtualMedia(ctx context.Context, installationID int, source string, keepIDs []string, libraryIDs []int) (VirtualReconcileResult, error) {
	return r.reconcileVirtualMedia(ctx, installationID, source, keepIDs, libraryIDs, nil)
}

// ReconcileVirtualMediaVerified is the completeness-gated reconciliation entry
// point used by the core virtual-library monitor. Unlike ReconcileVirtualMedia,
// destructive reconciliation requires evidence that the keep set came from a
// pass that enumerated the entire queue. A partial pass, a queue that lost
// items, or a keep set that shrinks implausibly is refused and logged instead
// of deleting live media.
func (r *VirtualMediaRegistrar) ReconcileVirtualMediaVerified(ctx context.Context, installationID int, source string, keepIDs []string, libraryIDs []int, evidence VirtualReconcileEvidence) (VirtualReconcileResult, error) {
	return r.reconcileVirtualMedia(ctx, installationID, source, keepIDs, libraryIDs, &evidence)
}

func (r *VirtualMediaRegistrar) reconcileVirtualMedia(ctx context.Context, installationID int, source string, keepIDs []string, libraryIDs []int, evidence *VirtualReconcileEvidence) (VirtualReconcileResult, error) {
	var result VirtualReconcileResult
	if r == nil || r.pool == nil {
		return result, errors.New("virtual catalog is unavailable")
	}
	if installationID < 0 || source == "" {
		return result, errors.New("installation and source are required")
	}
	if err := validateVirtualText("source", source, maxVirtualSourceBytes, true, false); err != nil {
		return result, err
	}
	// pgx encodes a nil slice as SQL NULL. `x <> ALL(NULL)`/`NOT (x =
	// ANY(NULL))` is NULL, not true, which would make reconciliation silently
	// retain every stale row when a plugin omits keep_media_ids or libraries.
	// Pass concrete empty arrays so the predicates have the intended semantics.
	keepIDs = normalizeVirtualKeepIDs(keepIDs)
	libraryIDs = normalizeVirtualLibraryIDs(libraryIDs)
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return result, fmt.Errorf("begin virtual reconciliation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockVirtualMediaInstallation(ctx, tx, installationID); err != nil {
		return result, err
	}
	if err := lockVirtualMediaSource(ctx, tx, installationID, source); err != nil {
		return result, err
	}
	// Safety guard: an empty keep list with existing claims means the plugin
	// lost its monitored state (restart, file corruption, provider outage).
	// Wiping everything would remove all virtual URIs for users, so refuse.
	// Item-scoped sources (e.g. "request:...") represent single requests and
	// may be legitimately withdrawn with an empty keep list.
	if len(keepIDs) == 0 && !strings.HasPrefix(source, "request:") {
		var existingCount int
		if err := tx.QueryRow(ctx, `
			SELECT COUNT(*) FROM virtual_media_source_claims
			WHERE plugin_installation_id=$1 AND source_key=$2
			  AND (cardinality($3::int[])=0 OR media_folder_id=ANY($3::int[]))`,
			installationID, source, libraryIDs).Scan(&existingCount); err != nil {
			return result, fmt.Errorf("virtual reconciliation guard check: %w", err)
		}
		if existingCount > 0 {
			slog.ErrorContext(ctx, "virtual reconciliation refused: empty keep list",
				"installation_id", installationID, "source", source, "existing_claims", existingCount)
			return result, fmt.Errorf("%w: empty keep list but %d existing claims exist for source %q — monitored state may be unavailable", ErrVirtualReconcileRefused, existingCount, source)
		}
	}
	if evidence != nil {
		// Positive completeness evidence is required before anything is
		// deleted. A keep set that merely happens to be non-empty is not
		// evidence: a resumed pass or a queue that lost items produces a
		// non-empty subset that would otherwise sweep live media.
		if !evidence.FullCycle || evidence.SourceCount <= 0 || evidence.QueueCount < evidence.SourceCount {
			slog.ErrorContext(ctx, "virtual reconciliation refused: incomplete enumeration evidence",
				"installation_id", installationID, "source", source,
				"full_cycle", evidence.FullCycle, "source_items", evidence.SourceCount,
				"queue_items", evidence.QueueCount, "keep_ids", len(keepIDs))
			return result, fmt.Errorf("%w: source %q had %d items in a %d-item queue (full cycle %t); a partial enumeration cannot authorize deletion",
				ErrVirtualReconcileRefused, source, evidence.SourceCount, evidence.QueueCount, evidence.FullCycle)
		}
		existing, removable, err := virtualSourceClaimCounts(ctx, tx, installationID, source, keepIDs, libraryIDs)
		if err != nil {
			return result, fmt.Errorf("virtual reconciliation completeness check: %w", err)
		}
		if removable >= reconcileImplausibleRemovalFloor && removable*2 > existing {
			slog.ErrorContext(ctx, "virtual reconciliation refused: keep set shrinks implausibly",
				"installation_id", installationID, "source", source,
				"existing_claims", existing, "removable_claims", removable, "keep_ids", len(keepIDs))
			return result, fmt.Errorf("%w: source %q would remove %d of %d existing claims; the keep set looks truncated",
				ErrVirtualReconcileRefused, source, removable, existing)
		}
	}
	if _, err := tx.Exec(ctx, `
		CREATE TEMP TABLE stale_virtual_source_claims ON COMMIT DROP AS
		SELECT plugin_installation_id,source_key,content_id,media_folder_id
		FROM virtual_media_source_claims
		WHERE plugin_installation_id=$1
		  AND source_key=$2
		  AND NOT (content_id=ANY($3::text[]))
		  AND (cardinality($4::int[])=0 OR media_folder_id=ANY($4::int[]))`,
		installationID, source, keepIDs, libraryIDs); err != nil {
		return result, fmt.Errorf("identify stale virtual source claims: %w", err)
	}
	itemRows, err := tx.Query(ctx, `SELECT DISTINCT content_id FROM stale_virtual_source_claims ORDER BY content_id`)
	if err != nil {
		return result, err
	}
	itemIDs, err := pgx.CollectRows(itemRows, pgx.RowTo[string])
	if err != nil {
		return result, err
	}
	for _, id := range itemIDs {
		if err := requestlock.LockItem(ctx, tx, id); err != nil {
			return result, err
		}
	}
	if _, err := tx.Exec(ctx, `
		CREATE TEMP TABLE stale_virtual_file_claims ON COMMIT DROP AS
		SELECT vmfsc.plugin_installation_id,vmfsc.content_id,vmfsc.media_folder_id,vmfsc.file_path
		FROM virtual_media_file_source_claims vmfsc
		JOIN stale_virtual_source_claims stale
		  ON stale.plugin_installation_id=vmfsc.plugin_installation_id
		 AND stale.source_key=vmfsc.source_key
		 AND stale.content_id=vmfsc.content_id
		 AND stale.media_folder_id=vmfsc.media_folder_id`); err != nil {
		return result, fmt.Errorf("identify stale virtual file claims: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM virtual_media_source_claims vmsc
		USING stale_virtual_source_claims stale
		WHERE vmsc.plugin_installation_id=stale.plugin_installation_id
		  AND vmsc.source_key=stale.source_key
		  AND vmsc.content_id=stale.content_id
		  AND vmsc.media_folder_id=stale.media_folder_id`); err != nil {
		return result, fmt.Errorf("delete stale virtual source claims: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM virtual_media_file_source_claims vmfsc
		USING stale_virtual_source_claims stale
		WHERE vmfsc.plugin_installation_id=stale.plugin_installation_id
		  AND vmfsc.source_key=stale.source_key
		  AND vmfsc.content_id=stale.content_id
		  AND vmfsc.media_folder_id=stale.media_folder_id`); err != nil {
		return result, fmt.Errorf("delete stale virtual file source claims: %w", err)
	}
	fileTag, err := tx.Exec(ctx, `
		DELETE FROM media_files mf
		USING stale_virtual_file_claims stale
		WHERE mf.content_id=stale.content_id
		  AND mf.media_folder_id=stale.media_folder_id
		  AND mf.file_path=stale.file_path
		  AND mf.virtual_owner_installation_id=stale.plugin_installation_id
		  AND (mf.container='virtual' OR mf.file_path LIKE 'virtual://%')
		  AND NOT EXISTS(
			SELECT 1 FROM virtual_media_file_source_claims remaining
			WHERE remaining.plugin_installation_id=stale.plugin_installation_id
			  AND remaining.content_id=stale.content_id
			  AND remaining.media_folder_id=stale.media_folder_id
			  AND remaining.file_path=stale.file_path
		  )`+virtualFileRetentionGuard)
	if err != nil {
		return result, fmt.Errorf("delete unclaimed virtual files: %w", err)
	}
	result.FilesRemoved = int(fileTag.RowsAffected())
	// Re-elect exactly one remaining metadata owner for each virtual-only item.
	// Preserve the current owner when it survives; otherwise promote the newest
	// source deterministically. Physical media always remains authoritative.
	if _, err := tx.Exec(ctx, `
		WITH chosen AS (
			SELECT DISTINCT ON (remaining.content_id)
				remaining.plugin_installation_id,remaining.source_key,
				remaining.content_id,remaining.media_folder_id
			FROM virtual_media_source_claims remaining
			WHERE remaining.content_id IN(SELECT content_id FROM stale_virtual_source_claims)
			  AND NOT EXISTS (
				SELECT 1 FROM media_files physical
				WHERE physical.content_id=remaining.content_id
				  AND physical.container<>'virtual'
				  AND physical.file_path NOT LIKE 'virtual://%'
			  )
			ORDER BY remaining.content_id,remaining.owns_item_metadata DESC,
			         remaining.last_seen_at DESC,
			         remaining.plugin_installation_id,remaining.source_key,remaining.media_folder_id
		)
		UPDATE virtual_media_source_claims remaining
		SET owns_item_metadata=(
			chosen.plugin_installation_id IS NOT NULL
			AND remaining.plugin_installation_id=chosen.plugin_installation_id
			AND remaining.source_key=chosen.source_key
			AND remaining.media_folder_id=chosen.media_folder_id
		),updated_at=NOW()
		FROM (SELECT DISTINCT content_id FROM stale_virtual_source_claims) affected
		LEFT JOIN chosen ON chosen.content_id=affected.content_id
		WHERE remaining.content_id=affected.content_id`); err != nil {
		return result, fmt.Errorf("promote remaining virtual metadata ownership: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE media_items mi SET
			virtual_owner_installation_id=owner.plugin_installation_id,
			virtual_source=COALESCE(owner.source_key,''),
			virtual_last_seen_at=(
				SELECT MAX(remaining.last_seen_at)
				FROM virtual_media_source_claims remaining
				WHERE remaining.content_id=mi.content_id
			),
			updated_at=NOW()
		FROM (
			SELECT affected.content_id,
			       chosen.plugin_installation_id,chosen.source_key
			FROM (SELECT DISTINCT content_id FROM stale_virtual_source_claims) affected
			LEFT JOIN LATERAL (
				SELECT remaining.plugin_installation_id,remaining.source_key
				FROM virtual_media_source_claims remaining
				WHERE remaining.content_id=affected.content_id
				  AND remaining.owns_item_metadata
				ORDER BY remaining.last_seen_at DESC,remaining.plugin_installation_id,remaining.source_key
				LIMIT 1
			) chosen ON true
		) owner
		WHERE mi.content_id=owner.content_id`); err != nil {
		return result, fmt.Errorf("refresh compatibility virtual ownership: %w", err)
	}
	rows, err := tx.Query(ctx, `
		DELETE FROM media_items mi
		WHERE mi.content_id IN(SELECT content_id FROM stale_virtual_source_claims)
		  AND NOT EXISTS (SELECT 1 FROM media_files mf WHERE mf.content_id = mi.content_id)
		  AND NOT EXISTS (SELECT 1 FROM library_collection_items lci WHERE lci.media_item_id = mi.content_id)
		  AND NOT EXISTS (SELECT 1 FROM virtual_media_source_claims vmsc WHERE vmsc.content_id=mi.content_id)
		  AND NOT EXISTS (SELECT 1 FROM episodes ep WHERE ep.series_id = mi.content_id)`+virtualItemRetentionGuard+`
		RETURNING mi.content_id`)
	if err != nil {
		return result, fmt.Errorf("delete stale virtual media: %w", err)
	}
	deletedIDs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return result, fmt.Errorf("collect stale virtual media IDs: %w", err)
	}
	if err := EnqueueSearchIndexDeletes(ctx, tx, deletedIDs); err != nil {
		return result, fmt.Errorf("enqueue stale virtual media search deletes: %w", err)
	}
	result.ItemsRemoved = len(deletedIDs)
	if err := tx.Commit(ctx); err != nil {
		return result, fmt.Errorf("commit virtual reconciliation: %w", err)
	}
	return result, nil
}

// RemoveVirtualMediaInstallation removes virtual catalog state owned by an
// installation. The caller must execute this in the same transaction as the
// installation row deletion so a failed uninstall cannot leave a partial
// cleanup behind.
func RemoveVirtualMediaInstallation(ctx context.Context, tx pgx.Tx, installationID int) (VirtualReconcileResult, error) {
	var result VirtualReconcileResult
	if installationID <= 0 {
		return result, errors.New("installation must be positive")
	}
	if err := lockVirtualMediaInstallation(ctx, tx, installationID); err != nil {
		return result, err
	}

	if _, err := tx.Exec(ctx, `
		CREATE TEMP TABLE removed_virtual_source_claims ON COMMIT DROP AS
		SELECT plugin_installation_id,source_key,content_id,media_folder_id
		FROM virtual_media_source_claims
		WHERE plugin_installation_id=$1`, installationID); err != nil {
		return result, fmt.Errorf("identify removed virtual source claims: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		CREATE TEMP TABLE removed_virtual_files ON COMMIT DROP AS
		SELECT mf.id,mf.content_id,mf.media_folder_id,mf.file_path
		FROM media_files mf
		WHERE (mf.container='virtual' OR mf.file_path LIKE 'virtual://%')
		  AND (
			mf.virtual_owner_installation_id=$1
			OR (
				COALESCE(mf.virtual_owner_installation_id,0)=0
				AND (
					EXISTS (
						SELECT 1 FROM virtual_media_file_source_claims claim
						WHERE claim.plugin_installation_id=$1
						  AND claim.content_id=mf.content_id
						  AND claim.media_folder_id=mf.media_folder_id
						  AND claim.file_path=mf.file_path
					)
					OR EXISTS (
						SELECT 1 FROM removed_virtual_source_claims claim
						WHERE claim.plugin_installation_id=$1
						  AND claim.content_id=mf.content_id
						  AND claim.media_folder_id=mf.media_folder_id
					)
					OR EXISTS (
						SELECT 1 FROM media_items item
						WHERE item.content_id=mf.content_id
						  AND item.virtual_owner_installation_id=$1
					)
				)
			)
		  )`, installationID); err != nil {
		return result, fmt.Errorf("identify removed virtual files: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		CREATE TEMP TABLE affected_virtual_content ON COMMIT DROP AS
		SELECT content_id FROM removed_virtual_source_claims
		UNION
		SELECT content_id FROM removed_virtual_files
		UNION
		SELECT content_id FROM media_items
		WHERE virtual_owner_installation_id=$1`, installationID); err != nil {
		return result, fmt.Errorf("identify affected virtual media: %w", err)
	}
	// Upserts lock the canonical content ID while changing virtual ownership.
	// Take the same locks before deleting files or claims so uninstall cannot
	// interleave with a registration for one of the affected items.
	// Dual-lock old and new namespaces for rolling-deploy compatibility.
	if _, err := tx.Exec(ctx, `
		SELECT pg_advisory_xact_lock(hashtext(content_id))
		FROM affected_virtual_content ORDER BY content_id`); err != nil {
		return result, fmt.Errorf("lock affected virtual media: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		SELECT pg_advisory_xact_lock(hashtext('catalog:item:' || content_id))
		FROM affected_virtual_content ORDER BY content_id`); err != nil {
		return result, fmt.Errorf("lock affected virtual media: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		DELETE FROM virtual_media_file_source_claims
		WHERE plugin_installation_id=$1`, installationID); err != nil {
		return result, fmt.Errorf("delete virtual file source claims: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM virtual_media_source_claims
		WHERE plugin_installation_id=$1`, installationID); err != nil {
		return result, fmt.Errorf("delete virtual source claims: %w", err)
	}

	// A file can be claimed by multiple installations. Select one surviving
	// owner, or retain one unowned row when a collection still references it.
	// Capturing all rows owned by the installation also removes stale files that
	// lost their source claim before uninstall.
	if _, err := tx.Exec(ctx, `
		CREATE TEMP TABLE replacement_virtual_files ON COMMIT DROP AS
		WITH keys AS (
			SELECT DISTINCT content_id,media_folder_id,file_path
			FROM removed_virtual_files
		)
		SELECT DISTINCT ON (keys.content_id,keys.media_folder_id,keys.file_path)
			keys.content_id,keys.media_folder_id,keys.file_path,
			remaining.plugin_installation_id AS owner_installation_id
		FROM keys
		JOIN virtual_media_file_source_claims remaining
		  ON remaining.content_id=keys.content_id
		 AND remaining.media_folder_id=keys.media_folder_id
		 AND remaining.file_path=keys.file_path
		ORDER BY keys.content_id,keys.media_folder_id,keys.file_path,
			remaining.last_seen_at DESC,remaining.plugin_installation_id,remaining.source_key`); err != nil {
		return result, fmt.Errorf("identify surviving virtual file owners: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO replacement_virtual_files(content_id,media_folder_id,file_path,owner_installation_id)
		SELECT keys.content_id,keys.media_folder_id,keys.file_path,0
		FROM (
			SELECT DISTINCT content_id,media_folder_id,file_path
			FROM removed_virtual_files
		) keys
		WHERE NOT EXISTS (
			SELECT 1 FROM replacement_virtual_files replacement
			WHERE replacement.content_id=keys.content_id
			  AND replacement.media_folder_id=keys.media_folder_id
			  AND replacement.file_path=keys.file_path
		)
		AND EXISTS (
			SELECT 1 FROM library_collection_items membership
			WHERE membership.media_item_id=keys.content_id
		)`); err != nil {
		return result, fmt.Errorf("preserve collection virtual files: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		CREATE TEMP TABLE retained_virtual_files ON COMMIT DROP AS
		SELECT DISTINCT ON (removed.content_id,removed.media_folder_id,removed.file_path)
			removed.id,replacement.owner_installation_id
		FROM removed_virtual_files removed
		JOIN replacement_virtual_files replacement
		  ON replacement.content_id=removed.content_id
		 AND replacement.media_folder_id=removed.media_folder_id
		 AND replacement.file_path=removed.file_path
		WHERE NOT EXISTS (
			SELECT 1 FROM media_files existing
			WHERE existing.content_id=removed.content_id
			  AND existing.media_folder_id=removed.media_folder_id
			  AND existing.file_path=removed.file_path
			  AND existing.virtual_owner_installation_id=replacement.owner_installation_id
		)
		ORDER BY removed.content_id,removed.media_folder_id,removed.file_path,removed.id`); err != nil {
		return result, fmt.Errorf("select retained virtual files: %w", err)
	}
	fileTag, err := tx.Exec(ctx, `
		DELETE FROM media_files mf
		USING removed_virtual_files removed
		WHERE mf.id=removed.id
		  AND NOT EXISTS (
			SELECT 1 FROM retained_virtual_files retained
			WHERE retained.id=mf.id
		)`+virtualFileRetentionGuard)
	if err != nil {
		return result, fmt.Errorf("delete unclaimed virtual files: %w", err)
	}
	result.FilesRemoved = int(fileTag.RowsAffected())
	if _, err := tx.Exec(ctx, `
		UPDATE media_files mf
		SET virtual_owner_installation_id=retained.owner_installation_id,updated_at=NOW()
		FROM retained_virtual_files retained
		WHERE mf.id=retained.id`); err != nil {
		return result, fmt.Errorf("promote surviving virtual file ownership: %w", err)
	}

	// Re-elect metadata ownership for virtual-only items. Physical media stays
	// authoritative even when virtual source claims remain attached.
	if _, err := tx.Exec(ctx, `
		WITH chosen AS (
			SELECT DISTINCT ON (claim.content_id)
				claim.content_id,claim.plugin_installation_id,claim.source_key,
				claim.media_folder_id
			FROM virtual_media_source_claims claim
			JOIN affected_virtual_content affected ON affected.content_id=claim.content_id
			WHERE NOT EXISTS (
				SELECT 1 FROM media_files physical
				WHERE physical.content_id=claim.content_id
				  AND physical.container<>'virtual'
				  AND physical.file_path NOT LIKE 'virtual://%'
			)
			ORDER BY claim.content_id,claim.owns_item_metadata DESC,
				claim.last_seen_at DESC,claim.plugin_installation_id,claim.source_key
		)
		UPDATE virtual_media_source_claims claim
		SET owns_item_metadata=(chosen.plugin_installation_id IS NOT NULL
			AND claim.plugin_installation_id=chosen.plugin_installation_id
			AND claim.source_key=chosen.source_key
			AND claim.media_folder_id=chosen.media_folder_id),updated_at=NOW()
		FROM affected_virtual_content affected
		LEFT JOIN chosen ON chosen.content_id=affected.content_id
		WHERE claim.content_id=affected.content_id`); err != nil {
		return result, fmt.Errorf("promote surviving virtual metadata ownership: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		WITH chosen AS (
			SELECT DISTINCT ON (claim.content_id)
				claim.content_id,claim.plugin_installation_id,claim.source_key,
				claim.last_seen_at
			FROM virtual_media_source_claims claim
			JOIN affected_virtual_content affected ON affected.content_id=claim.content_id
			WHERE claim.owns_item_metadata
			ORDER BY claim.content_id,claim.last_seen_at DESC,claim.plugin_installation_id,claim.source_key
		)
		UPDATE media_items mi
		SET virtual_owner_installation_id=CASE
				WHEN physical.content_id IS NULL THEN chosen.plugin_installation_id
				ELSE NULL
			END,
			virtual_source=CASE
				WHEN physical.content_id IS NULL THEN COALESCE(chosen.source_key,'')
				ELSE ''
			END,
			virtual_last_seen_at=CASE
				WHEN physical.content_id IS NULL THEN chosen.last_seen_at
				ELSE NULL
			END,
			updated_at=NOW()
		FROM affected_virtual_content affected
		LEFT JOIN chosen ON chosen.content_id=affected.content_id
		LEFT JOIN LATERAL (
			SELECT mi_physical.content_id
			FROM media_files mi_physical
			WHERE mi_physical.content_id=affected.content_id
			  AND mi_physical.container<>'virtual'
			  AND mi_physical.file_path NOT LIKE 'virtual://%'
			LIMIT 1
		) physical ON true
		WHERE mi.content_id=affected.content_id`); err != nil {
		return result, fmt.Errorf("restore surviving virtual metadata ownership: %w", err)
	}

	rows, err := tx.Query(ctx, `
		DELETE FROM media_items mi
		USING affected_virtual_content affected
		WHERE mi.content_id=affected.content_id
		  AND NOT EXISTS (SELECT 1 FROM media_files mf WHERE mf.content_id=mi.content_id)
		  AND NOT EXISTS (SELECT 1 FROM library_collection_items lci WHERE lci.media_item_id=mi.content_id)
		  AND NOT EXISTS (
		      SELECT 1 FROM virtual_media_source_claims vmsc
		      WHERE vmsc.content_id=mi.content_id
		        AND vmsc.owns_item_metadata
		        AND vmsc.plugin_installation_id<>$1
		  )
		  AND NOT EXISTS (SELECT 1 FROM episodes ep WHERE ep.series_id = mi.content_id)`+virtualItemRetentionGuard+`
		RETURNING mi.content_id`, installationID)
	if err != nil {
		return result, fmt.Errorf("delete orphaned virtual media items: %w", err)
	}
	deletedIDs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return result, fmt.Errorf("collect deleted virtual media IDs: %w", err)
	}
	if err := EnqueueSearchIndexDeletes(ctx, tx, deletedIDs); err != nil {
		return result, fmt.Errorf("enqueue virtual media search deletes: %w", err)
	}
	result.ItemsRemoved = len(deletedIDs)
	return result, nil
}

// RemoveInstallationVirtualMedia runs installation cleanup in its own
// transaction for callers that do not also need to delete the installation row.
func (r *VirtualMediaRegistrar) RemoveInstallationVirtualMedia(ctx context.Context, installationID int) (VirtualReconcileResult, error) {
	var result VirtualReconcileResult
	if r == nil || r.pool == nil {
		return result, errors.New("virtual catalog is unavailable")
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return result, fmt.Errorf("begin virtual installation cleanup: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	result, err = RemoveVirtualMediaInstallation(ctx, tx, installationID)
	if err != nil {
		return result, err
	}
	if err := tx.Commit(ctx); err != nil {
		return result, fmt.Errorf("commit virtual installation cleanup: %w", err)
	}
	return result, nil
}

func lockVirtualMediaSource(ctx context.Context, tx pgx.Tx, installationID int, source string) error {
	lockKey := fmt.Sprintf("vio:virtual-media-source:%d:%s", installationID, source)
	if err := requestlock.LockVirtual(ctx, tx, lockKey); err != nil {
		return fmt.Errorf("lock virtual media source: %w", err)
	}
	return nil
}

func lockVirtualMediaInstallation(ctx context.Context, tx pgx.Tx, installationID int) error {
	lockKey := fmt.Sprintf("vio:virtual-media-installation:%d", installationID)
	if err := requestlock.LockVirtual(ctx, tx, lockKey); err != nil {
		return fmt.Errorf("lock virtual media installation: %w", err)
	}
	return nil
}

func normalizeVirtualKeepIDs(ids []string) []string {
	if ids == nil {
		return []string{}
	}
	return ids
}

func normalizeVirtualLibraryIDs(ids []int) []int {
	if ids == nil {
		return []int{}
	}
	return ids
}

// MissingVirtualMediaContentIDs returns the subset of contentIDs that no
// longer has a media_items row. Monitor callers use it to evict queue entries
// for media that was genuinely removed; a probe failure returns an error so the
// caller can leave the queue untouched rather than treat transient trouble as
// removal.
func (r *VirtualMediaRegistrar) MissingVirtualMediaContentIDs(ctx context.Context, contentIDs []string) (map[string]struct{}, error) {
	missing := make(map[string]struct{})
	if r == nil || r.pool == nil || len(contentIDs) == 0 {
		return missing, nil
	}
	rows, err := r.pool.Query(ctx, `
		SELECT probe.id
		FROM unnest($1::text[]) AS probe(id)
		WHERE NOT EXISTS (SELECT 1 FROM media_items mi WHERE mi.content_id=probe.id)`, contentIDs)
	if err != nil {
		return nil, fmt.Errorf("probe missing virtual media: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan missing virtual media: %w", err)
		}
		missing[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate missing virtual media: %w", err)
	}
	return missing, nil
}

// virtualSourceClaimCounts returns the number of existing claims for a source
// (optionally scoped to libraries) and how many of them the keep set omits.
// keepIDs and libraryIDs must be normalized concrete slices so the array
// predicates have their intended semantics.
func virtualSourceClaimCounts(ctx context.Context, tx pgx.Tx, installationID int, source string, keepIDs []string, libraryIDs []int) (existing, removable int, err error) {
	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*)::int,
		       COUNT(*) FILTER (WHERE NOT (content_id=ANY($3::text[])))::int
		FROM virtual_media_source_claims
		WHERE plugin_installation_id=$1 AND source_key=$2
		  AND (cardinality($4::int[])=0 OR media_folder_id=ANY($4::int[]))`,
		installationID, source, keepIDs, libraryIDs).Scan(&existing, &removable); err != nil {
		return 0, 0, fmt.Errorf("count virtual source claims: %w", err)
	}
	return existing, removable, nil
}

func validateVirtualMedia(in VirtualMedia) error {
	if in.MediaType != "movie" && in.MediaType != "series" {
		return fmt.Errorf("%w: media_type must be movie or series", ErrInvalidVirtualMedia)
	}
	if err := validateVirtualText("title", in.Title, maxVirtualTitleBytes, true, false); err != nil {
		return err
	}
	folderID, err := strconv.Atoi(in.LibraryID)
	if err != nil || folderID <= 0 || strconv.Itoa(folderID) != in.LibraryID {
		return fmt.Errorf("%w: library_id must be a canonical positive integer", ErrInvalidVirtualMedia)
	}
	if in.Source != "" && strings.TrimSpace(in.Source) != in.Source {
		return fmt.Errorf("%w: source must not have surrounding whitespace", ErrInvalidVirtualMedia)
	}
	if err := validateVirtualText("source", normalizedVirtualSource(in.Source), maxVirtualSourceBytes, true, false); err != nil {
		return err
	}
	if in.Year < 0 || in.Year > 9999 {
		return fmt.Errorf("%w: year is out of range", ErrInvalidVirtualMedia)
	}
	if err := validateVirtualText("overview", in.Overview, maxVirtualOverviewBytes, false, true); err != nil {
		return err
	}
	if err := validateVirtualText("poster_path", in.PosterPath, maxVirtualArtworkPathBytes, false, false); err != nil {
		return err
	}
	if err := validateVirtualText("backdrop_path", in.BackdropPath, maxVirtualArtworkPathBytes, false, false); err != nil {
		return err
	}
	if len(in.Genres) > maxVirtualGenres {
		return fmt.Errorf("%w: genres exceeds %d entries", ErrInvalidVirtualMedia, maxVirtualGenres)
	}
	for i, genre := range in.Genres {
		if err := validateVirtualText(fmt.Sprintf("genres[%d]", i), genre, maxVirtualGenreBytes, true, false); err != nil {
			return err
		}
	}
	if err := validateVirtualIdentifiers(in); err != nil {
		return err
	}
	if in.TMDBID == "" && in.TVDBID == "" && in.IMDbID == "" {
		return fmt.Errorf("%w: an external identifier is required", ErrInvalidVirtualMedia)
	}
	if in.RuntimeMinutes < 0 || in.RuntimeMinutes > 24*60 {
		return fmt.Errorf("%w: runtime_minutes is out of range", ErrInvalidVirtualMedia)
	}
	if len(in.Variants) > maxVirtualVariantsPerMedia {
		return fmt.Errorf("%w: variants exceeds %d entries", ErrInvalidVirtualMedia, maxVirtualVariantsPerMedia)
	}
	if len(in.Episodes) > maxVirtualEpisodes {
		return fmt.Errorf("%w: episodes exceeds %d entries", ErrInvalidVirtualMedia, maxVirtualEpisodes)
	}
	if in.MediaType == "movie" && in.VirtualURI == "" && len(in.Variants) == 0 {
		return fmt.Errorf("%w: movie VirtualURI or Variants are required", ErrInvalidVirtualMedia)
	}
	if in.MediaType == "series" && (in.VirtualURI != "" || len(in.Variants) > 0) {
		return fmt.Errorf("%w: series playback sources must be attached to episodes", ErrInvalidVirtualMedia)
	}
	if in.MediaType == "series" && len(in.Episodes) == 0 {
		return fmt.Errorf("%w: series Episodes are required", ErrInvalidVirtualMedia)
	}

	seenURIs := make(map[string]struct{})
	fileCount := 0
	if in.VirtualURI != "" {
		if err := validateCanonicalVirtualURI(in.VirtualURI, in.MediaType, 0, 0); err != nil {
			return fmt.Errorf("%w: VirtualURI: %w", ErrInvalidVirtualMedia, err)
		}
		seenURIs[in.VirtualURI] = struct{}{}
	}
	for i, variant := range in.Variants {
		if err := validateVirtualVariant(variant, in.MediaType, 0, 0); err != nil {
			return fmt.Errorf("%w: variants[%d]: %w", ErrInvalidVirtualMedia, i, err)
		}
		if _, duplicate := seenURIs[variant.VirtualURI]; duplicate {
			return fmt.Errorf("%w: duplicate virtual URI %q", ErrInvalidVirtualMedia, variant.VirtualURI)
		}
		seenURIs[variant.VirtualURI] = struct{}{}
		fileCount++
	}
	if in.MediaType == "movie" && len(in.Variants) == 0 {
		fileCount++
	}
	for i, episode := range in.Episodes {
		if in.MediaType != "series" {
			return fmt.Errorf("%w: episodes are only valid for series", ErrInvalidVirtualMedia)
		}
		if err := validateVirtualEpisode(episode, i, seenURIs); err != nil {
			return err
		}
		if len(episode.Variants) > 0 {
			fileCount += len(episode.Variants)
		} else if episode.VirtualURI != "" {
			fileCount++
		}
	}
	if fileCount > maxVirtualFilesPerRegistration {
		return fmt.Errorf("%w: registration exceeds %d virtual files", ErrInvalidVirtualMedia, maxVirtualFilesPerRegistration)
	}
	return nil
}

func normalizedVirtualSource(source string) string {
	source = strings.TrimSpace(source)
	if source == "" {
		return "plugin"
	}
	return source
}

func validateVirtualIdentifiers(in VirtualMedia) error {
	for name, value := range map[string]string{
		"imdb_id": in.IMDbID,
		"tmdb_id": in.TMDBID,
		"tvdb_id": in.TVDBID,
	} {
		if value == "" {
			continue
		}
		if len(value) > maxVirtualIdentifierBytes || strings.TrimSpace(value) != value {
			return fmt.Errorf("%w: %s is invalid", ErrInvalidVirtualMedia, name)
		}
		if name == "imdb_id" {
			if !imdbIDPattern.MatchString(value) {
				return fmt.Errorf("%w: imdb_id must be a canonical tt identifier", ErrInvalidVirtualMedia)
			}
			continue
		}
		if !numericProviderIDPattern.MatchString(value) {
			return fmt.Errorf("%w: %s must be a canonical positive integer", ErrInvalidVirtualMedia, name)
		}
	}
	return nil
}

func validateVirtualEpisode(ep VirtualEpisode, index int, seenURIs map[string]struct{}) error {
	if ep.SeasonNumber <= 0 || ep.EpisodeNumber <= 0 {
		return fmt.Errorf("%w: episodes[%d] must have positive season and episode numbers", ErrInvalidVirtualMedia, index)
	}
	if ep.SeasonNumber > 100000 || ep.EpisodeNumber > 100000 {
		return fmt.Errorf("%w: episodes[%d] season or episode number is out of range", ErrInvalidVirtualMedia, index)
	}
	if err := validateVirtualText(fmt.Sprintf("episodes[%d].title", index), ep.Title, maxVirtualTitleBytes, false, false); err != nil {
		return err
	}
	if err := validateVirtualText(fmt.Sprintf("episodes[%d].overview", index), ep.Overview, maxVirtualOverviewBytes, false, true); err != nil {
		return err
	}
	if err := validateVirtualText(fmt.Sprintf("episodes[%d].still_path", index), ep.StillPath, maxVirtualArtworkPathBytes, false, false); err != nil {
		return err
	}
	if ep.RuntimeMinutes < 0 || ep.RuntimeMinutes > 24*60 {
		return fmt.Errorf("%w: episodes[%d].runtime_minutes is out of range", ErrInvalidVirtualMedia, index)
	}
	if len(ep.Variants) > maxVirtualVariantsPerMedia {
		return fmt.Errorf("%w: episodes[%d].variants exceeds %d entries", ErrInvalidVirtualMedia, index, maxVirtualVariantsPerMedia)
	}
	if ep.VirtualURI != "" {
		if err := validateCanonicalVirtualURI(ep.VirtualURI, "series", ep.SeasonNumber, ep.EpisodeNumber); err != nil {
			return fmt.Errorf("%w: episodes[%d].VirtualURI: %w", ErrInvalidVirtualMedia, index, err)
		}
		if _, duplicate := seenURIs[ep.VirtualURI]; duplicate {
			return fmt.Errorf("%w: duplicate virtual URI %q", ErrInvalidVirtualMedia, ep.VirtualURI)
		}
		seenURIs[ep.VirtualURI] = struct{}{}
	}
	for variantIndex, variant := range ep.Variants {
		if err := validateVirtualVariant(variant, "series", ep.SeasonNumber, ep.EpisodeNumber); err != nil {
			return fmt.Errorf("%w: episodes[%d].variants[%d]: %w", ErrInvalidVirtualMedia, index, variantIndex, err)
		}
		if _, duplicate := seenURIs[variant.VirtualURI]; duplicate {
			return fmt.Errorf("%w: duplicate virtual URI %q", ErrInvalidVirtualMedia, variant.VirtualURI)
		}
		seenURIs[variant.VirtualURI] = struct{}{}
	}
	return nil
}

func validateVirtualVariant(variant VirtualMediaVariant, mediaType string, season, episode int) error {
	if err := validateCanonicalVirtualURI(variant.VirtualURI, mediaType, season, episode); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"label":        variant.Label,
		"resolution":   variant.Resolution,
		"codec_video":  variant.CodecVideo,
		"codec_audio":  variant.CodecAudio,
		"hdr":          variant.HDR,
		"container":    variant.Container,
		"source_type":  variant.SourceType,
		"availability": variant.Availability,
	} {
		limit := maxVirtualAttributeBytes
		if name == "label" {
			limit = maxVirtualVariantLabelBytes
		}
		if err := validateVirtualText(name, value, limit, false, false); err != nil {
			return err
		}
	}
	if variant.RuntimeMinutes < 0 || variant.RuntimeMinutes > 24*60 {
		return errors.New("runtime_minutes is out of range")
	}
	if variant.Bitrate < 0 || variant.Bitrate > 1_000_000_000 {
		return errors.New("bitrate is out of range")
	}
	if variant.FileSize < 0 {
		return errors.New("file_size cannot be negative")
	}
	if err := validateVirtualLanguages("audio_languages", variant.AudioLanguages); err != nil {
		return err
	}
	return validateVirtualLanguages("subtitle_languages", variant.SubtitleLanguages)
}

func validateVirtualLanguages(field string, languages []string) error {
	if len(languages) > maxVirtualLanguages {
		return fmt.Errorf("%s exceeds %d entries", field, maxVirtualLanguages)
	}
	for i, language := range languages {
		if err := validateVirtualText(fmt.Sprintf("%s[%d]", field, i), language, maxVirtualLanguageBytes, true, false); err != nil {
			return err
		}
	}
	return nil
}

func validateVirtualText(field, value string, maxBytes int, required, allowFormattingControls bool) error {
	if !utf8.ValidString(value) || len(value) > maxBytes || strings.IndexByte(value, 0) >= 0 {
		return fmt.Errorf("%w: %s is invalid or exceeds %d bytes", ErrInvalidVirtualMedia, field, maxBytes)
	}
	if strings.TrimSpace(value) != value {
		return fmt.Errorf("%w: %s must not have surrounding whitespace", ErrInvalidVirtualMedia, field)
	}
	if required && value == "" {
		return fmt.Errorf("%w: %s is required", ErrInvalidVirtualMedia, field)
	}
	for _, r := range value {
		if unicode.IsControl(r) && (!allowFormattingControls || (r != '\n' && r != '\r' && r != '\t')) {
			return fmt.Errorf("%w: %s contains control characters", ErrInvalidVirtualMedia, field)
		}
	}
	return nil
}

func validateCanonicalVirtualURI(raw, mediaType string, season, episode int) error {
	if raw == "" || len(raw) > maxVirtualURIBytes || strings.TrimSpace(raw) != raw || !utf8.ValidString(raw) {
		return fmt.Errorf("URI is empty, malformed, or exceeds %d bytes", maxVirtualURIBytes)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse URI: %w", err)
	}
	if parsed.Scheme != "virtual" || parsed.Opaque != "" || parsed.User != nil || parsed.Fragment != "" {
		return errors.New("URI must use the canonical virtual:// scheme without userinfo or fragment")
	}
	if parsed.Host != mediaType || parsed.Hostname() != mediaType || parsed.Port() != "" {
		return fmt.Errorf("URI host must be %q", mediaType)
	}
	if parsed.RawPath != "" || strings.Contains(parsed.EscapedPath(), "%") || parsed.ForceQuery {
		return errors.New("URI path must use unescaped canonical segments")
	}
	segments := strings.Split(strings.TrimPrefix(parsed.Path, "/"), "/")
	expectedSegments := 1
	if season > 0 || episode > 0 {
		expectedSegments = 3
	}
	idOffset := 0
	if len(segments) == expectedSegments+1 {
		provider := segments[0]
		if provider != "imdb" && provider != "tmdb" && provider != "tvdb" {
			return errors.New("URI path contains an unsupported provider namespace")
		}
		if mediaType == "movie" && provider == "tvdb" {
			return errors.New("movie URI cannot use a TVDB namespace")
		}
		idOffset = 1
	}
	if len(segments) != expectedSegments+idOffset {
		return fmt.Errorf("URI path must contain %d canonical identifier segment(s)", expectedSegments)
	}
	for _, segment := range segments {
		if !virtualPathSegmentPattern.MatchString(segment) {
			return errors.New("URI path contains a non-canonical segment")
		}
	}
	identifier := segments[idOffset]
	if idOffset == 1 {
		switch segments[0] {
		case "imdb":
			if !imdbIDPattern.MatchString(identifier) {
				return errors.New("URI contains an invalid IMDb ID")
			}
		case "tmdb", "tvdb":
			if !numericProviderIDPattern.MatchString(identifier) {
				return errors.New("URI contains an invalid numeric provider ID")
			}
		}
	} else if !imdbIDPattern.MatchString(identifier) && !numericProviderIDPattern.MatchString(identifier) {
		return errors.New("URI contains an invalid external identifier")
	}
	if expectedSegments == 3 {
		if segments[idOffset+1] != strconv.Itoa(season) || segments[idOffset+2] != strconv.Itoa(episode) {
			return errors.New("URI season and episode must match the episode payload")
		}
	}
	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil {
		return fmt.Errorf("parse URI query: %w", err)
	}
	if parsed.RawQuery != query.Encode() {
		return errors.New("URI query must use canonical encoding and key order")
	}
	for key, values := range query {
		if key != "profile" && key != "result" && key != "results" {
			return fmt.Errorf("URI query parameter %q is not supported", key)
		}
		if len(values) != 1 || values[0] == "" || len(values[0]) > maxVirtualQueryValueBytes {
			return fmt.Errorf("URI query parameter %q must have one bounded value", key)
		}
		if err := validateVirtualText("URI query "+key, values[0], maxVirtualQueryValueBytes, true, false); err != nil {
			return err
		}
	}
	if _, result := query["result"]; result {
		if _, allResults := query["results"]; allResults {
			return errors.New("URI cannot contain both result and results")
		}
	}
	if values, ok := query["results"]; ok && values[0] != "all" {
		return errors.New(`URI results value must be "all"`)
	}
	return nil
}

func runtimeSeconds(minutes int) int {
	if minutes <= 0 {
		return 0
	}
	return minutes * 60
}

func virtualContentID(in VirtualMedia) string {
	if in.MediaType == "series" && in.TVDBID != "" {
		return "series-tvdb-" + in.TVDBID
	}
	if in.TMDBID != "" {
		return in.MediaType + "-tmdb-" + in.TMDBID
	}
	if in.IMDbID != "" {
		return in.MediaType + "-imdb-" + in.IMDbID
	}
	// No external ID available: build a deterministic fallback so distinct
	// items do not silently collide on the same content_id.
	h := sha256.Sum256([]byte(in.Source + "|" + in.MediaType + "|" + in.Title + "|" + strconv.Itoa(in.Year) + "|" + in.VirtualURI))
	return in.MediaType + "-hash-" + hex.EncodeToString(h[:12])
}

func virtualLibraryCompatible(folderType, mediaType string) bool {
	return folderType == "mixed" || (mediaType == "movie" && folderType == "movies") || (mediaType == "series" && folderType == "series")
}

func upsertVirtualEpisode(ctx context.Context, tx pgx.Tx, seriesID string, folderID, installationID int, ownsItemMetadata bool, seriesTemplate []ReleaseIdentity, ep VirtualEpisode) (bool, []string, error) {
	seasonID := fmt.Sprintf("%s-%d", strings.Replace(seriesID, "series-", "season-", 1), ep.SeasonNumber)
	episodeID := fmt.Sprintf("%s-%d-%d", strings.Replace(seriesID, "series-", "episode-", 1), ep.SeasonNumber, ep.EpisodeNumber)
	var persistedSeasonID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO seasons(content_id,series_id,season_number,title,air_date,metadata_source)
		VALUES($1,$2,$3,$4,$5,'provider')
		ON CONFLICT(series_id,season_number) DO UPDATE SET
			title=CASE WHEN $6 THEN EXCLUDED.title ELSE seasons.title END,
			air_date=CASE WHEN $6 THEN COALESCE(EXCLUDED.air_date,seasons.air_date) ELSE seasons.air_date END,
			updated_at=CASE WHEN $6 THEN NOW() ELSE seasons.updated_at END
		RETURNING content_id`,
		seasonID, seriesID, ep.SeasonNumber, fmt.Sprintf("Season %d", ep.SeasonNumber), nullTime(ep.AirDate), ownsItemMetadata,
	).Scan(&persistedSeasonID); err != nil {
		return false, nil, fmt.Errorf("upsert virtual season: %w", err)
	}
	var persistedEpisodeID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO episodes(
			content_id,series_id,season_id,season_number,episode_number,title,
			overview,air_date,runtime,still_path,metadata_source
		) VALUES($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8,NULLIF($9,0),NULLIF($10,''),'provider')
		ON CONFLICT(series_id,season_number,episode_number) DO UPDATE SET
			season_id=CASE WHEN $11 THEN EXCLUDED.season_id ELSE episodes.season_id END,
			title=CASE WHEN $11 AND EXCLUDED.title<>'' THEN EXCLUDED.title ELSE episodes.title END,
			overview=CASE WHEN $11 THEN COALESCE(EXCLUDED.overview,episodes.overview) ELSE episodes.overview END,
			air_date=CASE WHEN $11 THEN COALESCE(EXCLUDED.air_date,episodes.air_date) ELSE episodes.air_date END,
			runtime=CASE WHEN $11 THEN COALESCE(EXCLUDED.runtime,episodes.runtime) ELSE episodes.runtime END,
			still_path=CASE WHEN $11 THEN COALESCE(EXCLUDED.still_path,episodes.still_path) ELSE episodes.still_path END,
			updated_at=CASE WHEN $11 THEN NOW() ELSE episodes.updated_at END
		RETURNING content_id`,
		episodeID, seriesID, persistedSeasonID, ep.SeasonNumber, ep.EpisodeNumber,
		ep.Title, ep.Overview, nullTime(ep.AirDate), ep.RuntimeMinutes, ep.StillPath, ownsItemMetadata,
	).Scan(&persistedEpisodeID); err != nil {
		return false, nil, fmt.Errorf("upsert virtual episode: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO episode_libraries(episode_id,media_folder_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, persistedEpisodeID, folderID); err != nil {
		return false, nil, fmt.Errorf("link virtual episode: %w", err)
	}

	// Map the caller-resolved series template (incoming plus stored
	// aliases) onto this episode's coordinates.
	episodeIDs := make([]ReleaseIdentity, 0, len(seriesTemplate))
	for _, t := range seriesTemplate {
		t.SeasonNumber = ep.SeasonNumber
		t.EpisodeNumber = ep.EpisodeNumber
		if t.Validate() == nil {
			episodeIDs = append(episodeIDs, t)
		}
	}
	episodeIDs = sortReleaseIdentities(episodeIDs)
	// An active override on any alias wins outright; only with no active
	// override do local possession and provider/air-date evidence apply.
	// In particular a future override blocks even a locally proven episode
	// (existing physical files are preserved, new virtual files are not
	// created).
	eligible, overridden := false, false
	if len(episodeIDs) > 0 {
		// Lock the full alias set so a concurrent override write on any
		// alias serializes against the eligibility read below.
		if err := lockReleaseIdentitiesTx(ctx, tx, episodeIDs); err != nil {
			return false, nil, err
		}
		// Single captured decision at one evaluation time.
		entries, err := captureReleaseOverridesTx(ctx, tx, episodeIDs)
		if err != nil {
			return false, nil, err
		}
		if allowed, active := decideReleaseOverrides(time.Now().UTC(), entries); active {
			eligible, overridden = allowed, true
		}
	}
	if !overridden {
		var locallyProven bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS(SELECT 1 FROM episodes WHERE content_id=$1 AND metadata_source='local')
			    OR EXISTS(SELECT 1 FROM media_files WHERE episode_id=$1 AND container<>'virtual' AND file_path NOT LIKE 'virtual://%')`,
			persistedEpisodeID).Scan(&locallyProven); err != nil {
			return false, nil, fmt.Errorf("check virtual episode provenance: %w", err)
		}
		if locallyProven {
			eligible = true
		} else if err := tx.QueryRow(ctx, `SELECT COALESCE(verified_episode_release_at(series_id,season_number,episode_number,air_date::timestamp AT TIME ZONE 'UTC') <= statement_timestamp(),false) FROM episodes WHERE content_id=$1`, persistedEpisodeID).Scan(&eligible); err != nil {
			return false, nil, fmt.Errorf("check virtual episode release: %w", err)
		}
	}
	reason := ""
	if !eligible {
		reason = "future_date"
		if ep.AirDate.IsZero() {
			reason = "missing_date"
		}
	}
	if err := recordReleaseMetadata(ctx, tx, episodeIDs, reason); err != nil {
		return false, nil, err
	}
	if !eligible {
		return false, nil, nil
	}

	var createdPaths []string
	if len(ep.Variants) > 0 {
		for _, v := range ep.Variants {
			if err := upsertVirtualFileVariant(ctx, tx, seriesID, persistedEpisodeID, folderID, installationID, v, ep.RuntimeMinutes); err != nil {
				return false, nil, err
			}
			createdPaths = append(createdPaths, v.VirtualURI)
		}
		return true, createdPaths, nil
	}
	if ep.VirtualURI != "" {
		if err := upsertVirtualFileWithMeta(ctx, tx, seriesID, persistedEpisodeID, folderID, installationID, ep.VirtualURI, runtimeSeconds(ep.RuntimeMinutes), VirtualMedia{
			Resolution:        ep.Resolution,
			CodecVideo:        ep.CodecVideo,
			CodecAudio:        ep.CodecAudio,
			HDR:               ep.HDR,
			Bitrate:           ep.Bitrate,
			FileSize:          ep.FileSize,
			Container:         ep.Container,
			SourceType:        ep.SourceType,
			AudioLanguages:    ep.AudioLanguages,
			SubtitleLanguages: ep.SubtitleLanguages,
		}); err != nil {
			return false, nil, err
		}
		createdPaths = append(createdPaths, ep.VirtualURI)
		return true, createdPaths, nil
	}
	return false, nil, nil
}

//nolint:unused // Retained for compatibility with dormant integration paths.
func upsertVirtualFile(ctx context.Context, tx pgx.Tx, contentID, episodeID string, folderID, installationID int, uri string, duration int) error {
	return upsertVirtualFileWithMeta(ctx, tx, contentID, episodeID, folderID, installationID, uri, duration, VirtualMedia{})
}

func isUnplayableVirtualURI(uri string) bool {
	raw := strings.TrimSpace(strings.ToLower(uri))
	if strings.HasPrefix(raw, "virtual://series/") || strings.HasPrefix(raw, "virtual://show/") {
		parsed, err := url.Parse(raw)
		if err == nil {
			trimmed := strings.Trim(parsed.Path, "/")
			if trimmed == "" {
				return true
			}
			parts := strings.Split(trimmed, "/")
			if len(parts) < 3 {
				return true
			}
		}
	}
	return false
}

// virtualResultParamPattern strips the volatile result parameter from a
// virtual URI so rotations of the same release can be recognized as the same
// file regardless of the hash the provider attached this time.
var virtualResultParamPattern = regexp.MustCompile(`[?&]result=[^&]*`)

// neutralVirtualMediaPath returns uri with every result= query parameter
// removed, collapsing candidate rotations onto one stable identity.
func neutralVirtualMediaPath(uri string) string {
	neutral := virtualResultParamPattern.ReplaceAllString(uri, "")
	return strings.TrimRight(neutral, "?&")
}

// adoptVirtualFileRow finds an existing virtual row for the same content and
// neutral path (ignoring the volatile result= hash) so a rotation can update
// that row in place instead of inserting a sibling whose new ID invalidates
// client-cached media versions. Returns the adopted row ID, or 0.
func adoptVirtualFileRow(ctx context.Context, tx pgx.Tx, contentID, episodeID string, folderID, installationID int, neutralURI, editionLabel string) int {
	var adoptID int
	err := tx.QueryRow(ctx, `
		SELECT id FROM media_files
		WHERE content_id=$1
		  AND COALESCE(episode_id,'') = COALESCE(NULLIF($2,''),'')
		  AND virtual_owner_installation_id=$3
		  AND media_folder_id=$4
		  AND regexp_replace(file_path, '[?&]result=[^&]*', '', 'g') = $5
		  AND COALESCE(edition_raw,'') = $6
		ORDER BY id
		LIMIT 1`,
		contentID, episodeID, installationID, folderID, neutralURI, editionLabel).Scan(&adoptID)
	if err != nil {
		return 0
	}
	return adoptID
}

// dropVirtualSiblingRows removes any other rows sharing the neutral path after
// an adoption, so superseded hashes don't linger as phantom versions.
func dropVirtualSiblingRows(ctx context.Context, tx pgx.Tx, contentID, episodeID string, folderID, installationID, keepID int, neutralURI, editionLabel string) {
	_, _ = tx.Exec(ctx, `
		DELETE FROM media_files mf
		WHERE mf.content_id=$1
		  AND COALESCE(mf.episode_id,'') = COALESCE(NULLIF($2,''),'')
		  AND mf.virtual_owner_installation_id=$3
		  AND mf.media_folder_id=$4
		  AND mf.id <> $5
		  AND regexp_replace(mf.file_path, '[?&]result=[^&]*', '', 'g') = $6
		  AND COALESCE(mf.edition_raw,'') = $7`+virtualFileRetentionGuard,
		contentID, episodeID, installationID, folderID, keepID, neutralURI, editionLabel)
}

func upsertVirtualFileWithMeta(ctx context.Context, tx pgx.Tx, contentID, episodeID string, folderID, installationID int, uri string, duration int, in VirtualMedia) error {
	if isUnplayableVirtualURI(uri) {
		return nil
	}
	if err := requestlock.LockVirtual(ctx, tx, uri); err != nil {
		return err
	}
	isHDR := in.HDR != ""
	fileSize := in.FileSize
	if fileSize < 0 {
		fileSize = 0
	}
	audioLangs := in.AudioLanguages
	if audioLangs == nil {
		audioLangs = []string{}
	}
	// Collapse language aliases so one language never lands as two JSONB
	// subtitle tracks, whatever order the caller supplied.
	subLangs := stream.DedupeLanguageAliases(in.SubtitleLanguages)
	if subLangs == nil {
		subLangs = []string{}
	}
	// Rotation adoption: if a previous resolution of this same release stored
	// its (now stale) result hash under another row ID, refresh that row in
	// place — preserving the stable media file identity clients cache.
	neutralURI := neutralVirtualMediaPath(uri)
	if adoptID := adoptVirtualFileRow(ctx, tx, contentID, episodeID, folderID, installationID, neutralURI, ""); adoptID > 0 {
		if _, err := tx.Exec(ctx, `
			UPDATE media_files SET
				file_path=$4, file_size=$11, container=$12, media_folder_id=$3,
				probe_source=CASE
					WHEN probe_source='virtual_collection' THEN probe_source
					WHEN probe_updated_at IS NOT NULL THEN NULL
					ELSE 'virtual'
				END,
				-- Registration stores the row and its declared display hints; it
				-- is not a probe. A row a real probe already stamped is no longer
				-- verified once the hints replace its probed inventory, so clear
				-- the stamp and let the next play re-probe. Never-probed rows
				-- stay NULL; collection-owned rows keep their stamp.
				probe_updated_at=CASE
					WHEN probe_source='virtual_collection' THEN probe_updated_at
					ELSE NULL
				END,
				missing_since=NULL, updated_at=now(),
				resolution=NULLIF($6,''), codec_video=NULLIF($7,''), codec_audio=NULLIF($8,''),
				hdr=$9, bitrate=NULLIF($10,0),
				duration=CASE
					WHEN $5>0 THEN $5
					WHEN duration>0 THEN duration
					ELSE COALESCE((SELECT runtime * 60 FROM episodes WHERE content_id = NULLIF($2,'')),(SELECT runtime * 60 FROM media_items WHERE content_id = $1 AND NULLIF($2,'') IS NULL))
				END,
				audio_tracks=COALESCE((SELECT jsonb_agg(jsonb_build_object('language', x)) FROM unnest($13::text[]) x),'[]'::jsonb),
				subtitle_tracks=COALESCE((SELECT jsonb_agg(jsonb_build_object('language', x)) FROM unnest($14::text[]) x),'[]'::jsonb),
				virtual_owner_installation_id=COALESCE(NULLIF($16,0), virtual_owner_installation_id)
			WHERE id=$15`,
			contentID, episodeID, folderID, uri, duration, in.Resolution, in.CodecVideo, in.CodecAudio, isHDR, in.Bitrate, fileSize, "virtual", audioLangs, subLangs, adoptID, installationID); err != nil {
			return fmt.Errorf("adopt virtual file rotation: %w", err)
		}
		dropVirtualSiblingRows(ctx, tx, contentID, episodeID, folderID, installationID, adoptID, neutralURI, "")
		return nil
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO media_files(
			content_id,episode_id,media_folder_id,file_path,file_size,container,duration,probe_source,probe_updated_at,
			resolution,codec_video,codec_audio,hdr,bitrate,audio_tracks,subtitle_tracks,virtual_owner_installation_id
		) VALUES($1,NULLIF($2,''),$3,$4,$11,$12,COALESCE(NULLIF($5,0),(SELECT runtime * 60 FROM episodes WHERE content_id = NULLIF($2,'')),(SELECT runtime * 60 FROM media_items WHERE content_id = $1 AND NULLIF($2,'') IS NULL)),'virtual',NULL,NULLIF($6,''),NULLIF($7,''),NULLIF($8,''),$9,NULLIF($10,0),COALESCE((SELECT jsonb_agg(jsonb_build_object('language', x)) FROM unnest($13::text[]) x),'[]'::jsonb),COALESCE((SELECT jsonb_agg(jsonb_build_object('language', x)) FROM unnest($14::text[]) x),'[]'::jsonb),$15)
		ON CONFLICT (file_path, virtual_owner_installation_id, media_folder_id) WHERE virtual_owner_installation_id IS NOT NULL DO UPDATE SET
			content_id=EXCLUDED.content_id,
			episode_id=EXCLUDED.episode_id,
			media_folder_id=EXCLUDED.media_folder_id,
			file_size=EXCLUDED.file_size,
			container=EXCLUDED.container,
			probe_source=CASE
				WHEN media_files.probe_source='virtual_collection' THEN media_files.probe_source
				WHEN media_files.probe_updated_at IS NOT NULL THEN NULL
				ELSE 'virtual'
			END,
			-- Registration is not a probe, and it overwrites the target row's
			-- probed inventory with provider hints. A row a real probe had
			-- stamped is no longer verified: clear the stamp so the next play
			-- re-probes. Collection-owned rows keep their stamp.
			probe_updated_at=CASE
				WHEN media_files.probe_source='virtual_collection' THEN media_files.probe_updated_at
				ELSE NULL
			END,
			missing_since=NULL,
			updated_at=now(),
			resolution=EXCLUDED.resolution,
			codec_video=EXCLUDED.codec_video,
			codec_audio=EXCLUDED.codec_audio,
			hdr=EXCLUDED.hdr,
			bitrate=EXCLUDED.bitrate,
			duration=CASE
				WHEN EXCLUDED.duration > 0 THEN EXCLUDED.duration
				WHEN media_files.duration > 0 THEN media_files.duration
				ELSE COALESCE((SELECT runtime * 60 FROM episodes WHERE content_id = EXCLUDED.episode_id),(SELECT runtime * 60 FROM media_items WHERE content_id = EXCLUDED.content_id AND EXCLUDED.episode_id IS NULL))
			END,
			audio_tracks=EXCLUDED.audio_tracks,
			subtitle_tracks=EXCLUDED.subtitle_tracks,
			virtual_owner_installation_id=EXCLUDED.virtual_owner_installation_id`,
		contentID, episodeID, folderID, uri, duration, in.Resolution, in.CodecVideo, in.CodecAudio, isHDR, in.Bitrate, fileSize, "virtual", audioLangs, subLangs, installationID)
	if err != nil {
		return fmt.Errorf("upsert virtual file: %w", err)
	}
	return nil
}

func upsertVirtualFileVariant(ctx context.Context, tx pgx.Tx, contentID, episodeID string, folderID, installationID int, v VirtualMediaVariant, fallbackRuntimeMinutes int) error {
	if isUnplayableVirtualURI(v.VirtualURI) {
		return nil
	}
	if err := requestlock.LockVirtual(ctx, tx, v.VirtualURI); err != nil {
		return err
	}
	isHDR := v.HDR != ""
	fileSize := v.FileSize
	if fileSize < 0 {
		fileSize = 0
	}
	audioLangs := v.AudioLanguages
	if audioLangs == nil {
		audioLangs = []string{}
	}
	subLangs := stream.DedupeLanguageAliases(v.SubtitleLanguages)
	if subLangs == nil {
		subLangs = []string{}
	}
	effRuntimeMinutes := v.RuntimeMinutes
	if effRuntimeMinutes <= 0 {
		effRuntimeMinutes = fallbackRuntimeMinutes
	}
	// Rotation adoption for variants: refresh the sibling row that carries an
	// older result hash of the same edition in place, preserving its ID.
	neutralURI := neutralVirtualMediaPath(v.VirtualURI)
	if adoptID := adoptVirtualFileRow(ctx, tx, contentID, episodeID, folderID, installationID, neutralURI, v.Label); adoptID > 0 {
		if _, err := tx.Exec(ctx, `
			UPDATE media_files SET
				file_path=$4, file_size=$12, container=$13, media_folder_id=$3,
				probe_source=CASE
					WHEN probe_source='virtual_collection' THEN probe_source
					WHEN probe_updated_at IS NOT NULL THEN NULL
					ELSE 'virtual'
				END,
				-- Registration stores the row and its declared display hints; it
				-- is not a probe. A row a real probe already stamped is no longer
				-- verified once the hints replace its probed inventory, so clear
				-- the stamp and let the next play re-probe. Never-probed rows
				-- stay NULL; collection-owned rows keep their stamp.
				probe_updated_at=CASE
					WHEN probe_source='virtual_collection' THEN probe_updated_at
					ELSE NULL
				END,
				missing_since=NULL, updated_at=now(),
				resolution=NULLIF($6,''), codec_video=NULLIF($7,''), codec_audio=NULLIF($8,''),
				hdr=$9, bitrate=NULLIF($10,0), edition_raw=$11, release_name='', release_group='',
				duration=CASE
					WHEN $5>0 THEN $5
					WHEN duration>0 THEN duration
					ELSE COALESCE((SELECT runtime * 60 FROM episodes WHERE content_id = NULLIF($2,'')),(SELECT runtime * 60 FROM media_items WHERE content_id = $1 AND NULLIF($2,'') IS NULL))
				END,
				audio_tracks=COALESCE((SELECT jsonb_agg(jsonb_build_object('language', x)) FROM unnest($14::text[]) x),'[]'::jsonb),
				subtitle_tracks=COALESCE((SELECT jsonb_agg(jsonb_build_object('language', x)) FROM unnest($15::text[]) x),'[]'::jsonb)
			WHERE id=$16`,
			contentID, episodeID, folderID, v.VirtualURI, runtimeSeconds(effRuntimeMinutes), v.Resolution, v.CodecVideo, v.CodecAudio, isHDR, v.Bitrate, v.Label, fileSize, "virtual", audioLangs, subLangs, adoptID); err != nil {
			return fmt.Errorf("adopt virtual file variant rotation: %w", err)
		}
		dropVirtualSiblingRows(ctx, tx, contentID, episodeID, folderID, installationID, adoptID, neutralURI, v.Label)
		return nil
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO media_files(
			content_id,episode_id,media_folder_id,file_path,file_size,container,duration,probe_source,probe_updated_at,
			resolution,codec_video,codec_audio,hdr,bitrate,edition_raw,release_name,release_group,audio_tracks,subtitle_tracks,virtual_owner_installation_id
		) VALUES($1,NULLIF($2,''),$3,$4,$12,$13,COALESCE(NULLIF($5,0),(SELECT runtime * 60 FROM episodes WHERE content_id = NULLIF($2,'')),(SELECT runtime * 60 FROM media_items WHERE content_id = $1 AND NULLIF($2,'') IS NULL)),'virtual',NULL,NULLIF($6,''),NULLIF($7,''),NULLIF($8,''),$9,NULLIF($10,0),$11,'','',COALESCE((SELECT jsonb_agg(jsonb_build_object('language', x)) FROM unnest($14::text[]) x),'[]'::jsonb),COALESCE((SELECT jsonb_agg(jsonb_build_object('language', x)) FROM unnest($15::text[]) x),'[]'::jsonb),$16)
		ON CONFLICT (file_path, virtual_owner_installation_id, media_folder_id) WHERE virtual_owner_installation_id IS NOT NULL DO UPDATE SET
			content_id=EXCLUDED.content_id,
			episode_id=EXCLUDED.episode_id,
			media_folder_id=EXCLUDED.media_folder_id,
			file_size=EXCLUDED.file_size,
			container=EXCLUDED.container,
			probe_source=CASE
				WHEN media_files.probe_source='virtual_collection' THEN media_files.probe_source
				WHEN media_files.probe_updated_at IS NOT NULL THEN NULL
				ELSE 'virtual'
			END,
			-- Registration is not a probe, and it overwrites the target row's
			-- probed inventory with provider hints. A row a real probe had
			-- stamped is no longer verified: clear the stamp so the next play
			-- re-probes. Collection-owned rows keep their stamp.
			probe_updated_at=CASE
				WHEN media_files.probe_source='virtual_collection' THEN media_files.probe_updated_at
				ELSE NULL
			END,
			missing_since=NULL,
			updated_at=now(),
			resolution=EXCLUDED.resolution,
			codec_video=EXCLUDED.codec_video,
			codec_audio=EXCLUDED.codec_audio,
			hdr=EXCLUDED.hdr,
			bitrate=EXCLUDED.bitrate,
			duration=CASE
				WHEN EXCLUDED.duration > 0 THEN EXCLUDED.duration
				WHEN media_files.duration > 0 THEN media_files.duration
				ELSE COALESCE((SELECT runtime * 60 FROM episodes WHERE content_id = EXCLUDED.episode_id),(SELECT runtime * 60 FROM media_items WHERE content_id = EXCLUDED.content_id AND EXCLUDED.episode_id IS NULL))
			END,
			edition_raw=EXCLUDED.edition_raw,
			release_name=EXCLUDED.release_name,
			release_group=EXCLUDED.release_group,
			audio_tracks=EXCLUDED.audio_tracks,
			subtitle_tracks=EXCLUDED.subtitle_tracks`,
		contentID, episodeID, folderID, v.VirtualURI, runtimeSeconds(effRuntimeMinutes), v.Resolution, v.CodecVideo, v.CodecAudio, isHDR, v.Bitrate, v.Label, fileSize, "virtual", audioLangs, subLangs, installationID)
	if err != nil {
		return fmt.Errorf("upsert virtual file variant: %w", err)
	}
	return nil
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

func (r *VirtualMediaRegistrar) fillEpisodeReleaseDates(ctx context.Context, in VirtualMedia) VirtualMedia {
	tmdbID, err := strconv.Atoi(in.TMDBID)
	if err != nil || tmdbID <= 0 || r.EpisodeReleaseDates == nil {
		return in
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	seasons := make(map[int]map[int]time.Time)
	for i := range in.Episodes {
		ep := &in.Episodes[i]
		if !ep.AirDate.IsZero() || ep.SeasonNumber <= 0 || ep.EpisodeNumber <= 0 {
			continue
		}
		dates, found := seasons[ep.SeasonNumber]
		if !found {
			if len(seasons) >= 20 {
				break
			}
			dates, err = r.EpisodeReleaseDates.EpisodeReleaseDates(ctx, tmdbID, ep.SeasonNumber)
			if err != nil {
				dates = nil
			}
			seasons[ep.SeasonNumber] = dates
		}
		ep.AirDate = dates[ep.EpisodeNumber]
	}
	return in
}

func (r *VirtualMediaRegistrar) normalizeSeriesVirtualMedia(ctx context.Context, in VirtualMedia) VirtualMedia {
	if in.MediaType != "series" {
		return in
	}
	hasTopLevelURI := in.VirtualURI != ""
	hasTopLevelVariants := len(in.Variants) > 0

	if !hasTopLevelURI && !hasTopLevelVariants && len(in.Episodes) > 0 {
		return in
	}

	contentID := virtualContentID(in)

	if len(in.Episodes) == 0 && r != nil && r.pool != nil {
		rows, err := r.pool.Query(ctx, `
			SELECT season_number, episode_number, title, COALESCE(overview,''), air_date, COALESCE(still_path,'')
			FROM episodes
			WHERE series_id=$1 AND season_number > 0 AND episode_number > 0
			ORDER BY season_number, episode_number`, contentID)
		queryHealthy := err == nil
		if queryHealthy {
			for rows.Next() {
				var ep VirtualEpisode
				var airDate *time.Time
				if err := rows.Scan(&ep.SeasonNumber, &ep.EpisodeNumber, &ep.Title, &ep.Overview, &airDate, &ep.StillPath); err == nil {
					if airDate != nil {
						ep.AirDate = *airDate
					}
					in.Episodes = append(in.Episodes, ep)
				}
			}
			if rowsErr := rows.Err(); rowsErr != nil {
				queryHealthy = false
				in.Episodes = nil
			}
			rows.Close()
		}
		if !queryHealthy {
			// The backfill read failed: leave episodes empty rather than
			// persisting a synthetic S01E01 placeholder built from nothing.
			// The registrar retries with real metadata on the next pass.
			return in
		}
	}

	if len(in.Episodes) == 0 {
		in.Episodes = []VirtualEpisode{{
			SeasonNumber:  1,
			EpisodeNumber: 1,
			Title:         in.Title,
		}}
	}

	baseURI := in.VirtualURI
	if baseURI == "" && hasTopLevelVariants {
		baseURI = in.Variants[0].VirtualURI
	}
	if baseURI == "" {
		baseURI = virtualPlaybackItemURIFromIDs("series", in.IMDbID, in.TMDBID, in.TVDBID)
	}

	for i := range in.Episodes {
		ep := &in.Episodes[i]
		if len(ep.Variants) == 0 {
			if hasTopLevelVariants {
				epVariants := make([]VirtualMediaVariant, 0, len(in.Variants))
				for _, v := range in.Variants {
					epV := v
					epV.VirtualURI = buildEpisodeURI(v.VirtualURI, ep.SeasonNumber, ep.EpisodeNumber)
					epVariants = append(epVariants, epV)
				}
				ep.Variants = epVariants
				ep.VirtualURI = ""
			} else if ep.VirtualURI == "" && baseURI != "" {
				ep.VirtualURI = buildEpisodeURI(baseURI, ep.SeasonNumber, ep.EpisodeNumber)
			}
		}
	}

	in.VirtualURI = ""
	in.Variants = nil

	return in
}

func virtualPlaybackItemURIFromIDs(mediaType, imdbID, tmdbID, tvdbID string) string {
	if imdbID != "" && imdbIDPattern.MatchString(imdbID) {
		return "virtual://" + mediaType + "/" + imdbID
	}
	if tvdbID != "" && numericProviderIDPattern.MatchString(tvdbID) {
		return "virtual://" + mediaType + "/tvdb/" + tvdbID
	}
	if tmdbID != "" && numericProviderIDPattern.MatchString(tmdbID) {
		return "virtual://" + mediaType + "/tmdb/" + tmdbID
	}
	return ""
}

func buildEpisodeURI(seriesURI string, season, episode int) string {
	parsed, err := url.Parse(seriesURI)
	if err != nil || parsed.Scheme != "virtual" || parsed.Host != "series" {
		return seriesURI
	}
	segments := strings.Split(strings.TrimPrefix(parsed.Path, "/"), "/")
	if len(segments) == 0 || (len(segments) == 1 && segments[0] == "") {
		return seriesURI
	}
	idOffset := 0
	if segments[0] == "imdb" || segments[0] == "tmdb" || segments[0] == "tvdb" {
		idOffset = 1
	}
	if len(segments) >= 3+idOffset {
		return seriesURI
	}
	basePath := strings.TrimPrefix(parsed.Path, "/")
	episodePath := fmt.Sprintf("/%s/%d/%d", basePath, season, episode)
	epURL := &url.URL{
		Scheme:   "virtual",
		Host:     "series",
		Path:     episodePath,
		RawQuery: parsed.RawQuery,
	}
	return epURL.String()
}

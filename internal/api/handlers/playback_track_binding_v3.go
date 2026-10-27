package handlers

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/subtitles"
)

// The plan mints selected track identities against the effective media file
// row, while a client re-sends the requested row id as start/replan file_id and
// echoes the plan's identities unchanged on a restart. Virtual candidate
// resolution can also serve a row other than the requested one. Validating an
// echoed identity against the requested id alone therefore discards a perfectly
// valid selection as "stale", which silently falls back to the default track.
//
// The helpers below normalize such an identity onto the row the request actually
// validates against, but only when the embedding row provably names the same
// candidate (or is one of the same session's committed rows for a restart).
// A row naming a different release stays foreign and is still dropped by the
// stale-identity guard.

// rekeyedTrackSelectionV3 records one explicit track identity whose ordinal was
// minted against carry. Resolution may serve a candidate with a different
// inventory, so the ordinal is only reusable once it has been checked against
// the served tracks; active is false for a malformed identity or a foreign row
// the stale guard must still drop.
type rekeyedTrackSelectionV3 struct {
	active  bool
	ordinal int
	carry   carriedInventorySourceV3
}

// carriedInventorySourceV3 is the inventory a carried track ordinal was minted
// against, together with the downloaded subtitle entries that inventory
// published. A models.MediaFile has no representation for downloaded tracks, so
// the file alone cannot prove an ordinal past its embedded/external inventory
// still names the same downloaded row after the repository list changed under
// the same id.
type carriedInventorySourceV3 struct {
	file *models.MediaFile
	// downloaded is the published downloaded tail in combined-ordinal order. It
	// is only meaningful when downloadedKnown is set: an unknown tail means the
	// evidence predates the capture and the live repository list is the
	// fallback, while a known-empty tail is genuine evidence the source
	// published none.
	downloaded      []playback.SubtitleInventoryItemV3
	downloadedKnown bool
	// subtitleHistorical marks file's subtitle layout as captured evidence (a
	// durable plan or a session snapshot) rather than the current catalog row.
	// A historical layout cannot prove what a downloaded ordinal names today
	// without the captured tail: the repository list may have been rewritten
	// under the same row. A current layout, by contrast, is the list that
	// published the selection.
	subtitleHistorical bool
	// subtitleEvidenceErr records a dependency failure while recovering the
	// captured downloaded tail. A selection that needs the tail must surface
	// the failure instead of interpreting the ordinal against unverified
	// evidence.
	subtitleEvidenceErr error
}

// subtitleSelectionOrdinalV3 returns the combined subtitle ordinal a request
// carries, from either the index or the identity.
func subtitleSelectionOrdinalV3(file *models.MediaFile, request *playback.StartRequestV3) (int, bool) {
	if file == nil || request == nil {
		return 0, false
	}
	if request.SubtitleTrackIndex != nil {
		return *request.SubtitleTrackIndex, true
	}
	if request.SubtitleTrackID == "" {
		return 0, false
	}
	_, kind, ordinal, ok := playback.ParseTrackIDV3(request.SubtitleTrackID)
	if !ok || kind != subtitleTrackKindV3 {
		return 0, false
	}
	return ordinal, true
}

// namesDownloadedSubtitleV3 reports whether the request's subtitle selection
// addresses the published downloaded tail of the carry source rather than one
// of its embedded or external tracks.
func (c carriedInventorySourceV3) namesDownloadedSubtitleV3(request *playback.StartRequestV3) bool {
	ordinal, ok := subtitleSelectionOrdinalV3(c.file, request)
	return ok && c.namesDownloadedOrdinalV3(ordinal)
}

// namesDownloadedOrdinalV3 reports whether ordinal addresses the published
// downloaded tail of the carry source.
func (c carriedInventorySourceV3) namesDownloadedOrdinalV3(ordinal int) bool {
	location, classified := classifySubtitleIndexV3(c.file, ordinal)
	return classified && location.source == playback.SubtitleSourceDownloadedV3
}

// downloadedSubtitleCarryIDV3 returns the durable downloaded-subtitle row a
// captured inventory item names. The in-memory row id is lost when a plan is
// decoded from JSON, but the sync key serialized alongside it names the same
// row.
func downloadedSubtitleCarryIDV3(item playback.SubtitleInventoryItemV3) int {
	if id := item.DownloadedSubtitleIDV3(); id > 0 {
		return id
	}
	if id, _, ok := subtitles.ParseSyncKey(item.SyncKey); ok {
		return id
	}
	return 0
}

// downloadedTailChangedV3 reports whether target still publishes the captured
// downloaded rows in the same order. The downloaded segment is the part of a
// plan inventory a file-shaped comparison cannot see: a row added, removed, or
// reordered behind an unchanged id rewrites every ordinal after it, and an
// unprovable identity counts as a change so the caller remaps instead of
// reusing a position it cannot verify.
func (h *PlaybackHandler) downloadedTailChangedV3(ctx context.Context, carry carriedInventorySourceV3, target *models.MediaFile) (bool, error) {
	if !carry.downloadedKnown || target == nil {
		return false, nil
	}
	live, err := h.listDownloadedSubtitlesV3(ctx, target)
	if err != nil {
		return false, err
	}
	if len(carry.downloaded) != len(live) {
		return true, nil
	}
	for index, item := range carry.downloaded {
		if id := downloadedSubtitleCarryIDV3(item); id <= 0 || id != live[index].DownloadedSubtitleID {
			return true, nil
		}
	}
	return false, nil
}

// startTrackRekeysV3 is the per-kind state captured before virtual resolution
// replaces the requested row with the served candidate.
type startTrackRekeysV3 struct {
	audio    rekeyedTrackSelectionV3
	subtitle rekeyedTrackSelectionV3
}

func (r startTrackRekeysV3) empty() bool {
	return !r.audio.active && !r.subtitle.active
}

// trackIdentityRekeyV3 rewrites a well-formed track identity of kind to fileID
// while preserving its ordinal. It reports false for a malformed identity or one
// of another kind, leaving the caller's value untouched.
func trackIdentityRekeyV3(fileID int, kind, trackID string) (string, bool) {
	if fileID <= 0 || trackID == "" {
		return trackID, false
	}
	embeddedFileID, embeddedKind, ordinal, ok := playback.ParseTrackIDV3(trackID)
	if !ok || embeddedKind != kind {
		return trackID, false
	}
	if embeddedFileID == fileID {
		return trackID, true
	}
	return playback.TrackIDV3(fileID, kind, ordinal), true
}

// virtualRowsNameSameReleaseV3 reports whether two catalog rows may describe
// bytes of the same release. Exact candidate equality covers a plan row and its
// requested sibling; a provider-neutral requested row names no concrete pick, so
// any concrete result candidate under its profile scope is a possible effective
// row for it. Two concrete rows with different result ids are different
// candidates and stay foreign.
func virtualRowsNameSameReleaseV3(requested, candidate *models.MediaFile) bool {
	if requested == nil || candidate == nil {
		return false
	}
	if !isVirtualPlaybackFile(requested) || !isVirtualPlaybackFile(candidate) {
		return false
	}
	if sameVirtualCandidate(requested.FilePath, candidate.FilePath) {
		return true
	}
	return virtualResultCandidateID(requested.FilePath) == "" &&
		sameVirtualProfileScope(requested.FilePath, candidate.FilePath)
}

// predecessorPlanInventoryV3 is the track inventory a predecessor attempt's
// plan was minted against. The catalog row under the same id can be re-probed
// in place, so it is not proof of the historical ordering; only the attempt's
// captured plan inventory is. A nil source slice means the record predates that
// inventory and the caller may fall back to the mutable catalog row.
type predecessorPlanInventoryV3 struct {
	audio      []models.AudioTrack
	audioKnown bool
	// subtitleItems is the durable published inventory. It is rebuilt into
	// file-shaped layouts against the row it is compared with so the source
	// fields the wire inventory does not carry (external path, embedded
	// container index/ID) can be recovered from the uncooked row.
	subtitleItems []playback.SubtitleInventoryItemV3
	subtitleKnown bool
}

func (i predecessorPlanInventoryV3) anyKnown() bool {
	return i.audioKnown || i.subtitleKnown
}

// restartCommittedEvidenceV3 collects the catalog rows a same-client restart
// may legitimately echo in a track identity — the predecessor attempt's
// committed requested/effective rows and the uniquely matching live session's
// effective, requested and evidence rows — together with the durable inventory
// each accepted row minted its identities against. An ambiguous live match
// carries nothing, exactly like the restart selection carry.
type restartCommittedEvidenceV3 struct {
	rows    map[int]bool
	durable map[int]predecessorPlanInventoryV3
	session *playback.Session
}

func (h *PlaybackHandler) restartCommittedEvidenceV3(ctx context.Context, req *playback.StartRequestV3, userID int, profileID, clientIP, userAgent string) restartCommittedEvidenceV3 {
	evidence := restartCommittedEvidenceV3{
		rows:    map[int]bool{},
		durable: map[int]predecessorPlanInventoryV3{},
	}
	if h == nil || req == nil {
		return evidence
	}
	h.addPredecessorCommittedRowIDsV3(ctx, req, userID, profileID, evidence.rows, evidence.durable)
	lister, ok := h.sessionMgr.(interface {
		GetUserSessions(userID int) []*playback.Session
	})
	if !ok {
		return evidence
	}
	previous := findCarryCandidateSessionV3(lister.GetUserSessions(userID), profileID, req.FileID, clientIP, userAgent)
	if previous == nil {
		return evidence
	}
	for _, id := range []int{previous.MediaFileID, previous.RequestedMediaFileID, previous.VirtualSubtitleEvidenceFileID} {
		if id > 0 {
			evidence.rows[id] = true
		}
	}
	evidence.session = previous
	return evidence
}

// addPredecessorCommittedRowIDsV3 adds the committed requested and effective
// rows of the predecessor attempt this request names, when that attempt belongs
// to the same user, profile and requested file. The attempt's plan inventory is
// recorded for the effective row (the row its selected identities were minted
// against), and for the requested row when they are the same row.
func (h *PlaybackHandler) addPredecessorCommittedRowIDsV3(ctx context.Context, req *playback.StartRequestV3, userID int, profileID string, rows map[int]bool, durable map[int]predecessorPlanInventoryV3) {
	predecessorID := req.PreviousPlaybackAttemptID
	if predecessorID == "" || h.PlanStoreV3 == nil {
		return
	}
	record, err := h.PlanStoreV3.GetAttemptByPlaybackAttemptID(ctx, predecessorID)
	if err != nil || record == nil ||
		record.UserID != userID || record.ProfileID != profileID ||
		record.RequestedMediaFileID != req.FileID {
		return
	}
	inventory := predecessorPlanInventoryFromRecordV3(record)
	if record.RequestedMediaFileID > 0 {
		rows[record.RequestedMediaFileID] = true
	}
	if record.EffectiveMediaFileID > 0 {
		rows[record.EffectiveMediaFileID] = true
	}
	if !inventory.anyKnown() {
		return
	}
	if record.EffectiveMediaFileID > 0 && durable != nil {
		durable[record.EffectiveMediaFileID] = inventory
	}
	if record.RequestedMediaFileID > 0 && record.RequestedMediaFileID == record.EffectiveMediaFileID && durable != nil {
		durable[record.RequestedMediaFileID] = inventory
	}
}

// predecessorPlanInventoryFromRecordV3 reads the durable track inventory out of
// a predecessor attempt's plan. A nil slice is absence (an older record), while
// a captured-empty slice is genuine evidence that the release had no track of
// that kind.
func predecessorPlanInventoryFromRecordV3(record *playback.AttemptRecordV3) predecessorPlanInventoryV3 {
	var inventory predecessorPlanInventoryV3
	if record == nil {
		return inventory
	}
	plan := record.CurrentPlan
	if plan.AudioTracks != nil {
		inventory.audioKnown = true
		inventory.audio = audioTracksFromInventoryV3(plan.AudioTracks)
	}
	if plan.Subtitle.Inventory != nil {
		inventory.subtitleKnown = true
		inventory.subtitleItems = plan.Subtitle.Inventory
	}
	return inventory
}

// replanDurableInventoryV3 reads the durable track inventory of the plan being
// replanned, keyed by the effective row its selected identities were minted
// against. A replan validates identities against the effective row, so that is
// the entry the rekey and the drift remap need; the requested row is included
// only when it is the same row, matching the predecessor-restart evidence.
func replanDurableInventoryV3(record *playback.AttemptRecordV3) map[int]predecessorPlanInventoryV3 {
	if record == nil || record.EffectiveMediaFileID <= 0 {
		return nil
	}
	inventory := predecessorPlanInventoryFromRecordV3(record)
	if !inventory.anyKnown() {
		return nil
	}
	durable := map[int]predecessorPlanInventoryV3{record.EffectiveMediaFileID: inventory}
	if record.RequestedMediaFileID > 0 && record.RequestedMediaFileID == record.EffectiveMediaFileID {
		durable[record.RequestedMediaFileID] = inventory
	}
	return durable
}

// subtitleLayoutFromPlanInventoryV3 rebuilds the external and embedded track
// arrays from a plan's published inventory. The published list is already
// deduplicated and dense, so its per-segment order is the source order the
// combined ordinal addresses. The wire inventory carries the identity fields a
// remap needs (language, codec/format, label, flags, default) but not the
// uncooked source fields. When live still names the same track at that
// published ordinal, its path / container index / container track ID are copied
// through so an unchanged row compares equal and a reorder still fails closed.
// Downloaded entries have no models track representation; they are returned as
// the opaque tail segment and stay addressable through the subtitle repository,
// exactly as the remap path expects.
func subtitleLayoutFromPlanInventoryV3(items []playback.SubtitleInventoryItemV3, live *models.MediaFile) ([]models.ExternalSubtitle, []models.SubtitleTrack, []playback.SubtitleInventoryItemV3) {
	var external []models.ExternalSubtitle
	var embedded []models.SubtitleTrack
	var downloaded []playback.SubtitleInventoryItemV3
	var liveInventory []playback.SubtitleInventoryItemV3
	if live != nil {
		liveInventory = playback.BuildSubtitleInventoryV3(live, nil)
	}
	for _, item := range items {
		switch item.Source {
		case playback.SubtitleSourceExternalV3:
			sub := models.ExternalSubtitle{
				Language:        item.Language,
				Format:          item.Codec,
				Title:           item.Label,
				Forced:          item.Forced,
				Default:         item.Default,
				HearingImpaired: item.HearingImpaired,
			}
			if source, ok := subtitlePlanLiveExternalV3(item, liveInventory, live); ok {
				sub.Path = source.Path
				sub.Title = source.Title
				sub.EmbeddedTitle = source.EmbeddedTitle
				sub.Resolution = source.Resolution
			}
			external = append(external, sub)
		case playback.SubtitleSourceEmbeddedV3:
			track := models.SubtitleTrack{
				Index:           subtitlePlanEmbeddedIndexV3(item),
				Language:        item.Language,
				Codec:           item.Codec,
				Title:           item.Label,
				Forced:          item.Forced,
				Default:         item.Default,
				HearingImpaired: item.HearingImpaired,
			}
			if source, ok := subtitlePlanLiveEmbeddedV3(item, liveInventory, live); ok {
				track.Index = source.Index
				track.ContainerTrackID = source.ContainerTrackID
				track.Title = source.Title
				track.EmbeddedTitle = source.EmbeddedTitle
				track.Resolution = source.Resolution
			}
			embedded = append(embedded, track)
		case playback.SubtitleSourceDownloadedV3:
			downloaded = append(downloaded, item)
		}
	}
	return external, embedded, downloaded
}

// subtitlePlanLiveExternalV3 returns the live sidecar the plan item still
// names, if any. Identity must agree on every attribute the plan published; a
// reordered or re-probed row therefore keeps the plan-derived values and lets
// the fingerprint report drift instead of borrowing the new path.
func subtitlePlanLiveExternalV3(item playback.SubtitleInventoryItemV3, liveInventory []playback.SubtitleInventoryItemV3, live *models.MediaFile) (models.ExternalSubtitle, bool) {
	if live == nil || item.CombinedIndex < 0 || item.CombinedIndex >= len(liveInventory) {
		return models.ExternalSubtitle{}, false
	}
	if liveInventory[item.CombinedIndex].Source != playback.SubtitleSourceExternalV3 {
		return models.ExternalSubtitle{}, false
	}
	sourceIndex, ok := playback.SubtitleInventoryOwnSourceIndexV3(live, item.CombinedIndex)
	if !ok || sourceIndex < 0 || sourceIndex >= len(live.ExternalSubtitles) {
		return models.ExternalSubtitle{}, false
	}
	source := live.ExternalSubtitles[sourceIndex]
	if !strings.EqualFold(source.Language, item.Language) ||
		!strings.EqualFold(source.Format, item.Codec) ||
		source.Forced != item.Forced || source.HearingImpaired != item.HearingImpaired ||
		!strings.EqualFold(subtitleExternalLiveLabelV3(source), item.Label) {
		return models.ExternalSubtitle{}, false
	}
	return source, true
}

// subtitlePlanLiveEmbeddedV3 is the embedded counterpart of
// subtitlePlanLiveExternalV3.
func subtitlePlanLiveEmbeddedV3(item playback.SubtitleInventoryItemV3, liveInventory []playback.SubtitleInventoryItemV3, live *models.MediaFile) (models.SubtitleTrack, bool) {
	if live == nil || item.CombinedIndex < 0 || item.CombinedIndex >= len(liveInventory) {
		return models.SubtitleTrack{}, false
	}
	if liveInventory[item.CombinedIndex].Source != playback.SubtitleSourceEmbeddedV3 {
		return models.SubtitleTrack{}, false
	}
	sourceIndex, ok := playback.SubtitleInventoryOwnSourceIndexV3(live, item.CombinedIndex)
	if !ok || sourceIndex < len(live.ExternalSubtitles) {
		return models.SubtitleTrack{}, false
	}
	sourceIndex -= len(live.ExternalSubtitles)
	if sourceIndex < 0 || sourceIndex >= len(live.SubtitleTracks) {
		return models.SubtitleTrack{}, false
	}
	source := live.SubtitleTracks[sourceIndex]
	if !strings.EqualFold(source.Language, item.Language) ||
		!strings.EqualFold(source.Codec, item.Codec) ||
		source.Forced != item.Forced || source.HearingImpaired != item.HearingImpaired ||
		!strings.EqualFold(subtitleEmbeddedLiveLabelV3(source), item.Label) {
		return models.SubtitleTrack{}, false
	}
	return source, true
}

func subtitleExternalLiveLabelV3(sub models.ExternalSubtitle) string {
	for _, value := range []string{sub.Title, sub.EmbeddedTitle, filepathBaseV3(sub.Path), sub.Language} {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func subtitleEmbeddedLiveLabelV3(track models.SubtitleTrack) string {
	for _, value := range []string{track.Title, track.EmbeddedTitle, track.Language} {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// subtitlePlanEmbeddedIndexV3 recovers the container stream index from the
// session-scoped sidecar URL the scoped plan inventory carries. Without it the
// published combined ordinal is only a menu position and must not be mistaken
// for the container index the extraction path addresses.
func subtitlePlanEmbeddedIndexV3(item playback.SubtitleInventoryItemV3) int {
	if item.URL == "" {
		return item.CombinedIndex
	}
	parsed, err := url.Parse(item.URL)
	if err != nil {
		return item.CombinedIndex
	}
	raw := parsed.Query().Get(playback.EmbeddedSubtitleStreamIndexParamV3)
	if raw == "" {
		return item.CombinedIndex
	}
	index, err := strconv.Atoi(raw)
	if err != nil || index < 0 {
		return item.CombinedIndex
	}
	return index
}

func filepathBaseV3(path string) string {
	if path == "" {
		return ""
	}
	return filepath.Base(path)
}

// currentInventorySourceV3 returns the live catalog row for fileID without
// overlaying a captured plan or session inventory. A fresh selection was minted
// against the inventory that published it, so predecessor ordering must not
// reinterpret its ordinal.
func (h *PlaybackHandler) currentInventorySourceV3(ctx context.Context, fileID int) carriedInventorySourceV3 {
	if h == nil || h.fileResolver == nil || fileID <= 0 {
		return carriedInventorySourceV3{}
	}
	row, err := h.fileResolver.GetByID(ctx, fileID)
	if err != nil || row == nil {
		return carriedInventorySourceV3{}
	}
	return carriedInventorySourceV3{file: row}
}

// rekeySourceFileWithInventoryV3 loads the inventory a track identity's ordinal
// was minted against. The catalog row is the base; when the id names the
// session's own effective or evidence row, the session's retained inventory is
// preferred because an in-place re-probe can rewrite the row under the same id.
// A predecessor plan's durable inventory wins over both because the request
// names that exact attempt: the row under the same id may have been re-probed
// since the identity was minted. The captured downloaded tail is returned
// alongside the file-shaped evidence because the latter cannot describe it.
func (h *PlaybackHandler) rekeySourceFileWithInventoryV3(ctx context.Context, fileID int, session *playback.Session, durable map[int]predecessorPlanInventoryV3) carriedInventorySourceV3 {
	if fileID <= 0 {
		return carriedInventorySourceV3{}
	}
	var source *models.MediaFile
	if h.fileResolver != nil {
		if row, err := h.fileResolver.GetByID(ctx, fileID); err == nil && row != nil {
			source = row
		}
	}
	if inventory, ok := durable[fileID]; ok {
		evidence := &models.MediaFile{ID: fileID}
		if source != nil {
			cloned := *source
			evidence = &cloned
		}
		evidence.ID = fileID
		carry := carriedInventorySourceV3{file: evidence}
		if inventory.audioKnown {
			evidence.AudioTracks = inventory.audio
		}
		if inventory.subtitleKnown {
			// Recover the uncooked source fields from the row that still names the
			// same inventory; the opaque downloaded tail is deliberately not mapped
			// onto file tracks but carried beside them.
			evidence.ExternalSubtitles, evidence.SubtitleTracks, carry.downloaded = subtitleLayoutFromPlanInventoryV3(inventory.subtitleItems, source)
			carry.downloadedKnown = true
			carry.subtitleHistorical = true
		}
		return carry
	}
	if session == nil || !session.VirtualSubtitleEvidenceSet {
		return carriedInventorySourceV3{file: source}
	}
	if fileID != session.MediaFileID && fileID != session.VirtualSubtitleEvidenceFileID {
		return carriedInventorySourceV3{file: source}
	}
	evidence := &models.MediaFile{ID: fileID}
	if source != nil {
		cloned := *source
		evidence = &cloned
	}
	evidence.AudioTracks = session.VirtualAudioTracks
	evidence.SubtitleTracks = session.VirtualSubtitleTracks
	evidence.ExternalSubtitles = session.VirtualExternalSubtitles
	carry := carriedInventorySourceV3{file: evidence, subtitleHistorical: true}
	// The session evidence arrays describe the file-shaped inventory but cannot
	// describe the downloaded tail. Without it a carried downloaded ordinal is
	// indistinguishable from an unchanged one and would be reused positionally
	// onto whatever row now occupies it. The session's durable attempt plan is
	// the same captured inventory the evidence was written from, so recover the
	// tail there. A dependency failure is carried, not swallowed: the selection
	// cannot be verified and the caller reports it instead of reusing an
	// ordinal it cannot prove.
	downloaded, known, tailErr := h.sessionEvidenceDownloadedTailV3(ctx, session, fileID)
	if tailErr != nil {
		carry.subtitleEvidenceErr = tailErr
	} else if known {
		carry.downloaded = downloaded
		carry.downloadedKnown = true
	}
	return carry
}

// sessionEvidenceDownloadedTailV3 returns the downloaded tail the session's
// durable attempt plan published for fileID. The plan inventory is only usable
// when it names the same effective row the session evidence covers; a plan for
// another row orders unrelated subtitles, so its tail is not evidence for this
// ordinal. A missing record and a record that lacks the inventory or names
// another row are absence of proof, not proof of an empty tail; the caller
// must fail closed for a historical downloaded ordinal. A store failure is a
// dependency error and is returned so the client can retry instead of being
// told the track is gone.
func (h *PlaybackHandler) sessionEvidenceDownloadedTailV3(ctx context.Context, session *playback.Session, fileID int) ([]playback.SubtitleInventoryItemV3, bool, error) {
	if h == nil || h.PlanStoreV3 == nil || session == nil || session.ID == "" || fileID <= 0 {
		return nil, false, nil
	}
	record, err := h.PlanStoreV3.GetAttempt(ctx, session.ID)
	if errors.Is(err, playback.ErrSessionNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, wrapSubtitleStoreErrorV3(err)
	}
	if record == nil {
		return nil, false, nil
	}
	if record.UserID != session.UserID || record.ProfileID != session.ProfileID || record.EffectiveMediaFileID != fileID {
		return nil, false, nil
	}
	if record.CurrentPlan.Subtitle.Inventory == nil {
		return nil, false, nil
	}
	_, _, downloaded := subtitleLayoutFromPlanInventoryV3(record.CurrentPlan.Subtitle.Inventory, nil)
	return downloaded, true, nil
}

// rekeyStartTrackIdentitiesV3 normalizes a start request's selected track
// identities onto req.FileID when the embedded row names the same virtual
// candidate, or is one of the same client's committed restart rows. It must run
// before NormalizeAndValidate, whose stale-identity drop otherwise erases the
// plan's own echoed identities. The returned state carries the inventory each
// echoed ordinal belongs to so constrainRekeyedStartTracksV3 can re-check it
// once virtual resolution has installed the served candidate's tracks.
func (h *PlaybackHandler) rekeyStartTrackIdentitiesV3(ctx context.Context, req *playback.StartRequestV3, userID int, profileID, clientIP, userAgent string) startTrackRekeysV3 {
	var captured startTrackRekeysV3
	if h == nil || h.fileResolver == nil || req == nil || req.FileID <= 0 {
		return captured
	}
	if req.AudioTrackID == "" && req.SubtitleTrackID == "" {
		return captured
	}
	requested, err := h.fileResolver.GetByID(ctx, req.FileID)
	if err != nil || requested == nil || !isVirtualPlaybackFile(requested) {
		return captured
	}
	committed := h.restartCommittedEvidenceV3(ctx, req, userID, profileID, clientIP, userAgent)
	rekey := func(kind, trackID string) (string, rekeyedTrackSelectionV3) {
		embeddedFileID, embeddedKind, ordinal, ok := playback.ParseTrackIDV3(trackID)
		if !ok || embeddedKind != kind {
			return trackID, rekeyedTrackSelectionV3{}
		}
		// The requested row itself is the target of any later normalization, but
		// its inventory still names what the ordinal means: a resolver
		// substitution can serve another candidate under this row, and the
		// ordinal must be re-checked against the served tracks. Prefer the
		// predecessor plan's captured inventory when this is a restart: the row
		// may have been re-probed in place since the identity was minted.
		if embeddedFileID == req.FileID {
			source := h.rekeySourceFileWithInventoryV3(ctx, embeddedFileID, committed.session, committed.durable)
			if source.file == nil {
				source = carriedInventorySourceV3{file: requested}
			}
			return trackID, rekeyedTrackSelectionV3{active: true, ordinal: ordinal, carry: source}
		}
		source := h.rekeySourceFileWithInventoryV3(ctx, embeddedFileID, committed.session, committed.durable)
		accepted := committed.rows[embeddedFileID]
		if !accepted && source.file != nil {
			accepted = virtualRowsNameSameReleaseV3(requested, source.file)
		}
		if !accepted {
			return trackID, rekeyedTrackSelectionV3{}
		}
		rekeyed, rekeyedOK := trackIdentityRekeyV3(req.FileID, kind, trackID)
		if !rekeyedOK {
			return trackID, rekeyedTrackSelectionV3{}
		}
		slog.InfoContext(ctx, "selected track identity re-keyed onto the requested row",
			"component", "playback", "kind", kind, "sent_track_id", trackID,
			"requested_file_id", req.FileID, "effective_file_id", embeddedFileID)
		return rekeyed, rekeyedTrackSelectionV3{active: true, ordinal: ordinal, carry: source}
	}
	req.AudioTrackID, captured.audio = rekey("audio", req.AudioTrackID)
	req.SubtitleTrackID, captured.subtitle = rekey("subtitle", req.SubtitleTrackID)
	return captured
}

// constrainRekeyedStartTracksV3 re-checks the identities captured before
// resolution against the candidate the start actually serves. The carried
// ordinal belongs to source's inventory; reusing it unchanged when resolution
// substitutes a candidate that orders its tracks differently silently plays
// another stream. The ordinal is therefore kept only for a proven matching
// inventory, remapped by identity otherwise, and reported unavailable (cleared,
// with a degradation warning added by the caller) when it cannot be proven.
func (h *PlaybackHandler) constrainRekeyedStartTracksV3(ctx context.Context, req *playback.StartRequestV3, target *models.MediaFile, captured startTrackRekeysV3) (audioUnavailable, subtitleUnavailable bool, err error) {
	if req == nil || target == nil || captured.empty() {
		return false, false, nil
	}
	if captured.audio.active {
		index, available := resolvedRekeyedAudioOrdinalV3(captured.audio.carry.file, target, captured.audio.ordinal)
		audioUnavailable = applyRekeyedAudioOrdinalV3(req, target, index, available)
		if audioUnavailable {
			slog.InfoContext(ctx, "re-keyed audio selection is unavailable on the resolved candidate; using the default track",
				"component", "playback", "from_audio_index", captured.audio.ordinal,
				"resolved_file_id", target.ID)
		}
	}
	if captured.subtitle.active {
		subtitleUnavailable, err = h.constrainRekeyedSubtitleSelectionV3(ctx, req, captured.subtitle.carry, target)
	}
	return audioUnavailable, subtitleUnavailable, err
}

// applyRekeyedAudioOrdinalV3 writes a re-keyed audio selection onto the request.
// A resolved ordinal rewrites the identity and keeps an accompanying index in
// step, because NormalizeAndValidate rejects an id/index pair naming two
// different tracks. An unavailable selection is cleared in both fields.
func applyRekeyedAudioOrdinalV3(req *playback.StartRequestV3, target *models.MediaFile, index int, available bool) (unavailable bool) {
	if req == nil || target == nil {
		return true
	}
	if !available {
		req.AudioTrackID = ""
		req.AudioTrackIndex = nil
		return true
	}
	req.AudioTrackID = playback.TrackIDV3(target.ID, "audio", index)
	if req.AudioTrackIndex != nil {
		resolved := index
		req.AudioTrackIndex = &resolved
	}
	return false
}

// resolvedRekeyedAudioOrdinalV3 returns the ordinal in target that names the
// same audio stream the source ordinal named. An identical inventory proves the
// ordinal still addresses the selected track; a different inventory is
// re-identified by the shared carried-index remap and fails closed when no
// unique equivalent exists. Both sides empty means neither inventory was probed:
// there is no contradictory track, so the caller's ordinal is kept for the
// downstream resolver exactly as before, rather than reporting a degradation the
// evidence cannot support.
func resolvedRekeyedAudioOrdinalV3(source, target *models.MediaFile, ordinal int) (int, bool) {
	if source == nil || target == nil {
		return 0, false
	}
	if playback.AudioLayoutsEqual(source.AudioTracks, target.AudioTracks) {
		if len(target.AudioTracks) == 0 || (ordinal >= 0 && ordinal < len(target.AudioTracks)) {
			return ordinal, true
		}
		return 0, false
	}
	remapped := remapCarriedAudioIndexV3(source.AudioTracks, ordinal, target)
	if remapped < 0 {
		return 0, false
	}
	return remapped, true
}

// constrainRekeyedSubtitleSelectionV3 keeps a re-keyed subtitle ordinal only
// when the served inventory matches the one it was minted against. Unlike
// audio, a changed inventory is remapped by the existing edition-switch
// language/format matcher; when no equivalent exists the selection is cleared
// and reported unavailable rather than reinterpreted against the new order.
// The downloaded tail counts as part of the inventory even though it has no
// file representation: a carried downloaded ordinal is re-checked against the
// captured rows even when the embedded/external tracks compare equal.
func (h *PlaybackHandler) constrainRekeyedSubtitleSelectionV3(ctx context.Context, req *playback.StartRequestV3, carry carriedInventorySourceV3, target *models.MediaFile) (unavailable bool, err error) {
	if req == nil {
		return false, nil
	}
	source := carry.file
	if source == nil {
		req.SubtitleTrackID = ""
		req.SubtitleTrackIndex = nil
		return true, nil
	}
	if carry.namesDownloadedSubtitleV3(req) {
		// A captured inventory cannot place a downloaded ordinal without the
		// tail it published: the repository list may have been rewritten under
		// the same row. Only a current-inventory selection, minted against the
		// live list that published it, is safe to interpret without one.
		if carry.subtitleEvidenceErr != nil {
			return false, carry.subtitleEvidenceErr
		}
		if carry.subtitleHistorical && !carry.downloadedKnown {
			slog.InfoContext(ctx, "re-keyed downloaded subtitle has no captured tail; clearing it",
				"component", "playback", "source_file_id", source.ID, "resolved_file_id", target.ID)
			req.SubtitleTrackID = ""
			req.SubtitleTrackIndex = nil
			return true, nil
		}
	}
	if sameSubtitleInventoryV3(source, target) {
		keep, keepErr := h.keepRekeyedSubtitleWithinSameInventoryV3(ctx, req, carry, target)
		if keepErr != nil {
			return false, keepErr
		}
		if keep {
			return false, nil
		}
	}
	if _, remapErr := h.remapSubtitleSelectionWithCarryV3(ctx, carry, target, req); remapErr != nil {
		if !errors.Is(remapErr, errSubtitleUnavailableInTargetV3) {
			// A failed subtitle-store lookup cannot prove the track is absent;
			// keeping the old ordinal would reinterpret it against an inventory
			// we never verified. Surface the dependency failure instead.
			return false, remapErr
		}
		// remapSubtitleSelectionV3 deliberately preserves the selection for a
		// caller that keeps hunting candidates. Here the effective row is
		// already chosen: a confirmed missing equivalent must clear both
		// fields and degrade, never leave the old ordinal to land on whatever
		// track now occupies it.
		slog.InfoContext(ctx, "re-keyed subtitle selection is unavailable on the resolved candidate; clearing it",
			"component", "playback", "source_file_id", source.ID, "resolved_file_id", target.ID)
		req.SubtitleTrackID = ""
		req.SubtitleTrackIndex = nil
		return true, nil
	}
	if req.SubtitleTrackID == "" {
		req.SubtitleTrackIndex = nil
		return true, nil
	}
	return false, nil
}

// keepRekeyedSubtitleWithinSameInventoryV3 decides whether a selection whose
// file-shaped source and target inventories compare equal may keep its ordinal
// without a remap. Non-downloaded tracks share the layout, so their ordinal
// still names the same track. A captured downloaded tail proves the downloaded
// segment unchanged. A current-inventory selection names the live list, so only
// a same-row ordinal is safe to keep; a cross-row one must be mapped by the
// list that published it.
func (h *PlaybackHandler) keepRekeyedSubtitleWithinSameInventoryV3(ctx context.Context, req *playback.StartRequestV3, carry carriedInventorySourceV3, target *models.MediaFile) (bool, error) {
	if !carry.namesDownloadedSubtitleV3(req) {
		return true, nil
	}
	if carry.downloadedKnown {
		changed, err := h.downloadedTailChangedV3(ctx, carry, target)
		if err != nil {
			return false, err
		}
		return !changed, nil
	}
	return carry.file != nil && carry.file.ID == target.ID, nil
}

// rekeyReplanTrackIdentitiesV3 is the replan counterpart: a serve association or
// candidate rotation can move the session's effective row without updating the
// durable record the replan seeds from, so the client's echoed plan identities
// may embed the session's row rather than the record's. Accept both when they
// name the same candidate or are rows the session itself carries, but re-check
// the carried ordinal against the effective row's inventory exactly like the
// start path: row acceptance alone does not prove the ordinal addresses the
// same stream, because a substituted candidate can reorder its tracks. durable
// carries the plan's captured inventory so an echoed identity on another
// committed row is interpreted against that generation, not a mutated catalog
// row. A same-row subtitle identity that names a downloaded track is the
// exception: the downloaded tail is invisible to the caller's file-shaped drift
// remap, so it is re-checked against the captured rows here — unless the caller
// already reconciled the ordinal, see subtitleSelectionCurrent.
func (h *PlaybackHandler) rekeyReplanTrackIdentitiesV3(ctx context.Context, start *playback.StartRequestV3, file *models.MediaFile, session *playback.Session, durable map[int]predecessorPlanInventoryV3, subtitleSelectionCurrent bool) (audioUnavailable, subtitleUnavailable bool, err error) {
	if h == nil || h.fileResolver == nil || start == nil || file == nil {
		return false, false, nil
	}
	if start.AudioTrackID == "" && start.SubtitleTrackID == "" {
		return false, false, nil
	}
	// Committed session rows are only interchangeable for a virtual binding:
	// two local editions are different files even when a serve association
	// once pointed the session at one of them.
	committed := map[int]bool{file.ID: true}
	virtualBinding := isVirtualPlaybackFile(file)
	if virtualBinding && session != nil {
		for _, id := range []int{session.MediaFileID, session.RequestedMediaFileID, session.VirtualSubtitleEvidenceFileID} {
			if id > 0 {
				committed[id] = true
			}
		}
	}
	acceptedSource := func(kind, trackID string) (carriedInventorySourceV3, int, bool) {
		// A same-row identity is not re-keyed here: it is still minted against
		// this row, and an in-place re-probe is repaired by the caller's drift
		// remap, which prefers the same durable inventory before this check. The
		// downloaded subtitle tail is the exception, because the drift remap
		// compares file-shaped inventories and cannot see it — but only for a
		// carried ordinal. subtitleSelectionCurrent marks a fresh choice from the
		// current menu, or a carried ordinal the drift remap already reconciled
		// into this row's inventory; either addresses the live list, so reading it
		// through the predecessor tail would name another downloaded row or clear
		// an appended one the old tail never published.
		embeddedFileID, embeddedKind, ordinal, ok := playback.ParseTrackIDV3(trackID)
		if !ok || embeddedKind != kind {
			return carriedInventorySourceV3{}, 0, false
		}
		if embeddedFileID == file.ID {
			if kind != subtitleTrackKindV3 || subtitleSelectionCurrent {
				return carriedInventorySourceV3{}, 0, false
			}
			source := h.rekeySourceFileWithInventoryV3(ctx, embeddedFileID, session, durable)
			if !source.downloadedKnown || !source.namesDownloadedOrdinalV3(ordinal) {
				return carriedInventorySourceV3{}, 0, false
			}
			return source, ordinal, true
		}
		source := h.rekeySourceFileWithInventoryV3(ctx, embeddedFileID, session, durable)
		accepted := committed[embeddedFileID]
		if !accepted && source.file != nil {
			accepted = virtualRowsNameSameReleaseV3(file, source.file)
		}
		if !accepted {
			return carriedInventorySourceV3{}, 0, false
		}
		// A fresh subtitle choice names the row whose live menu published it,
		// even when that row differs from the one this replan validates against.
		// Interpret the ordinal against that current inventory, then remap it
		// onto the target by identity; applying the predecessor inventory here
		// would read the viewer's new pick as whoever occupied the position in
		// the captured plan.
		if kind == subtitleTrackKindV3 && subtitleSelectionCurrent {
			source = h.currentInventorySourceV3(ctx, embeddedFileID)
		}
		return source, ordinal, true
	}
	if source, ordinal, ok := acceptedSource("audio", start.AudioTrackID); ok {
		sentTrackID := start.AudioTrackID
		index, available := resolvedRekeyedAudioOrdinalV3(source.file, file, ordinal)
		audioUnavailable = applyRekeyedAudioOrdinalV3(start, file, index, available)
		if audioUnavailable {
			slog.InfoContext(ctx, "replan audio selection is unavailable on the effective row; using the default track",
				"component", "playback", "from_audio_index", ordinal, "effective_file_id", file.ID)
		} else {
			slog.InfoContext(ctx, "replan selected audio track identity re-keyed onto the effective row",
				"component", "playback", "sent_track_id", sentTrackID,
				"effective_file_id", file.ID, "from_audio_index", ordinal, "to_audio_index", index)
		}
	}
	if source, ordinal, ok := acceptedSource("subtitle", start.SubtitleTrackID); ok {
		// Normalize to the target row first so the edition-switch matcher reads
		// the carried ordinal regardless of which committed row minted it.
		start.SubtitleTrackID = playback.TrackIDV3(file.ID, "subtitle", ordinal)
		subtitleUnavailable, err = h.constrainRekeyedSubtitleSelectionV3(ctx, start, source, file)
		if err != nil {
			return audioUnavailable, subtitleUnavailable, err
		}
		if !subtitleUnavailable {
			slog.InfoContext(ctx, "replan selected subtitle track identity re-keyed onto the effective row",
				"component", "playback", "effective_file_id", file.ID)
		}
	}
	return audioUnavailable, subtitleUnavailable, nil
}

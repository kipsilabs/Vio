package handlers

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/resolver"
	"github.com/jackc/pgx/v5"
)

// adoptionBoundTestRow builds a pinned virtual row for the bounded-refusal
// tests: a concrete ?result= pin, no probe stamp, and a durable video-hash tier.
func adoptionBoundTestRow(id int, hash string) *models.MediaFile {
	return &models.MediaFile{
		ID:                         id,
		ContentID:                  "episode-tvdb-471090-1-1",
		EpisodeID:                  "episode-tvdb-471090-1-1",
		FilePath:                   "virtual://series/tt39087958/1/1?profile=4K+Dolby+Vision&result=9d8e30b32858eab16eb4e1b5",
		MediaFolderID:              42,
		VirtualOwnerInstallationID: 7,
		ProviderVideoHash:          hash,
		ProviderReleaseName:        "Between.Steps.S01E01.2160p.WEB-DL",
		ProviderReleaseSize:        4_000_000_000,
	}
}

// TestVirtualAdoptionRefusalBoundSameReleaseIsNotTerminal proves the bound does
// not fire for a same-release renumber: when the resolver's explicit identity
// proves the candidate is the row's own release, the write is metadata-only and
// never reaches the terminal branch, however many refusals are on record.
func TestVirtualAdoptionRefusalBoundSameReleaseIsNotTerminal(t *testing.T) {
	row := adoptionBoundTestRow(5031897, "9d8e-hash")
	candidate := "virtual://series/tt39087958/1/1?profile=4K+Dolby+Vision&result=165c86960fd10b80f54ecf9c"
	probed := &models.MediaFile{AudioTracks: []models.AudioTrack{{Codec: "aac", Channels: 2}}}
	h := &PlaybackHandler{}
	key := virtualAdoptionRefusalKey(row.ID, row.FilePath)
	virtualAdoptionRefusals.clear(key)
	t.Cleanup(func() { virtualAdoptionRefusals.clear(key) })
	for i := 0; i < virtualAdoptionRefusalBound*2; i++ {
		virtualAdoptionRefusals.mark(key)
	}
	args, ok := h.virtualProbeEvidenceArgsForRow(context.Background(), row, candidate, probed,
		resolver.NewPersistedIdentityTiers("9d8e-hash", "", "", 0), true)
	if !ok {
		t.Fatal("same-release renumber was refused outright")
	}
	if args.RequireAdopt {
		t.Fatalf("same-release renumber was fenced: %+v", args)
	}
	if args.RefusalReason == virtualProbeRefusalAdoptionBoundExhausted {
		t.Fatalf("same-release renumber was named terminal: %+v", args)
	}
}

// TestVirtualAdoptionRefusalBoundInheritedIdentityStaysFenced is the blocker-1
// boundary: the transient's Provider* tiers are seeded from the requested row
// and only filled where empty, so a substituted candidate can inherit the pin's
// identity. An inherited equality on the probed file must never prove a release;
// only the resolver's explicit identity passed as `candidate` counts. With no
// explicit evidence the candidate stays foreign and fenced, and past the bound the
// refusal is named terminal.
func TestVirtualAdoptionRefusalBoundInheritedIdentityStaysFenced(t *testing.T) {
	row := adoptionBoundTestRow(5031903, "pin-hash")
	candidate := "virtual://series/tt39087958/1/1?profile=4K+Dolby+Vision&result=substituted"
	probed := &models.MediaFile{
		// Inherited from the requested row: identical to the pin, and precisely
		// the value that must not prove a release.
		ProviderVideoHash:   row.ProviderVideoHash,
		ProviderReleaseName: row.ProviderReleaseName,
		ProviderReleaseSize: row.ProviderReleaseSize,
	}
	h := &PlaybackHandler{}
	key := virtualAdoptionRefusalKey(row.ID, row.FilePath)
	virtualAdoptionRefusals.clear(key)
	t.Cleanup(func() { virtualAdoptionRefusals.clear(key) })
	for i := 0; i < virtualAdoptionRefusalBound; i++ {
		virtualAdoptionRefusals.mark(key)
	}
	args, ok := h.virtualProbeEvidenceArgsForRow(context.Background(), row, candidate, probed,
		resolver.PersistedIdentityTiers{}, true)
	if !ok {
		t.Fatal("args builder refused before the SQL fence could")
	}
	if !args.RequireAdopt {
		t.Fatalf("inherited identity disabled the adoption fence: %+v", args)
	}
	// The stored count no longer decides anything here: the terminal decision
	// belongs to the transactional refusal reconciliation, so the args builder
	// must not name the exhausted disposition on its own.
	if args.RefusalReason == virtualProbeRefusalAdoptionBoundExhausted {
		t.Fatalf("args builder made the terminal decision itself: %+v", args)
	}
}

// TestVirtualAdoptionRefusalBoundExplicitForeignIdentityStaysFenced proves the
// resolver's explicit identity is still checked for agreement: a candidate whose
// resolved identity shares no tier with the pin is foreign even past the bound.
func TestVirtualAdoptionRefusalBoundExplicitForeignIdentityStaysFenced(t *testing.T) {
	row := adoptionBoundTestRow(5031904, "pin-hash")
	candidate := "virtual://series/tt39087958/1/1?profile=4K+Dolby+Vision&result=foreign-explicit"
	probed := &models.MediaFile{}
	h := &PlaybackHandler{}
	key := virtualAdoptionRefusalKey(row.ID, row.FilePath)
	virtualAdoptionRefusals.clear(key)
	t.Cleanup(func() { virtualAdoptionRefusals.clear(key) })
	for i := 0; i < virtualAdoptionRefusalBound; i++ {
		virtualAdoptionRefusals.mark(key)
	}
	args, ok := h.virtualProbeEvidenceArgsForRow(context.Background(), row, candidate, probed,
		resolver.NewPersistedIdentityTiers("different-hash", "", "", 0), true)
	if !ok {
		t.Fatal("args builder refused before the SQL fence could")
	}
	if !args.RequireAdopt {
		t.Fatalf("explicitly foreign candidate disabled the fence: %+v", args)
	}
}

// TestVirtualAdoptionRefusalBoundForeignStaysFenced is the #300 fence half: a
// candidate that shares no durable identity tier with the pin is genuinely
// foreign. Past the refusal bound the write must still be fenced, and the
// refusal must be named terminal rather than looping.
func TestVirtualAdoptionRefusalBoundForeignStaysFenced(t *testing.T) {
	row := adoptionBoundTestRow(5031898, "row-hash")
	candidate := "virtual://series/tt39087958/1/1?profile=4K+Dolby+Vision&result=deadbeefcafe"
	probed := &models.MediaFile{
		ProviderVideoHash: "foreign-hash",
		AudioTracks:       []models.AudioTrack{{Codec: "aac", Channels: 2}},
	}
	h := &PlaybackHandler{
		VirtualFileMetadataSaver: func(context.Context, models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
			return VirtualFileMetadataUpdateResult{}, errors.New("should not be reached")
		},
	}
	key := virtualAdoptionRefusalKey(row.ID, row.FilePath)
	virtualAdoptionRefusals.clear(key)
	t.Cleanup(func() { virtualAdoptionRefusals.clear(key) })

	for i := 0; i < virtualAdoptionRefusalBound; i++ {
		virtualAdoptionRefusals.mark(key)
	}
	args, ok := h.virtualProbeEvidenceArgsForRow(context.Background(), row, candidate, probed, resolver.NewPersistedIdentityTiers("foreign-hash", "", "", 0), true)
	if !ok {
		t.Fatal("args builder refused the foreign write before the SQL fence could")
	}
	if !args.RequireAdopt {
		t.Fatalf("foreign release lost its adoption fence: %+v", args)
	}
	// The terminal disposition is decided by the transactional reconciliation,
	// not here: past the bound the args builder still only builds the fenced
	// write.
	if args.RefusalReason == virtualProbeRefusalAdoptionBoundExhausted {
		t.Fatalf("args builder made the terminal decision itself: %+v", args)
	}
}

// TestVirtualAdoptionRefusalBoundRotatesSameReleaseOwnerRow proves the other
// bounded rotation: a duplicate row of the same release already owns the live
// candidate path, so the pre-fix sibling guard refuses. Past the bound the probed
// evidence rotates onto that owner row (which already owns the path), landing a
// real stamp instead of looping on the requested row.
func TestVirtualAdoptionRefusalBoundRotatesSameReleaseOwnerRow(t *testing.T) {
	const (
		neutral   = "virtual://series/tt39087958/1/1"
		pinnedURI = neutral + "?result=pinned"
		ownerURI  = neutral + "?result=owner"
		content   = "episode-tvdb-471090-1-1"
	)
	pinnedRow := &models.MediaFile{
		ID: 5031897, ContentID: content, EpisodeID: content, FilePath: pinnedURI,
		MediaFolderID: 42, VirtualOwnerInstallationID: 7, ProbeSource: "virtual",
		ProviderVideoHash: "same-release-hash",
	}
	ownerRow := &models.MediaFile{
		ID: 5031899, ContentID: content, EpisodeID: content, FilePath: ownerURI,
		MediaFolderID: 42, VirtualOwnerInstallationID: 7, ProbeSource: "virtual",
		ProviderVideoHash: "same-release-hash",
	}
	h := &PlaybackHandler{
		fileResolver: byPathPlaybackFileResolver{
			testPlaybackFileResolver: testPlaybackFileResolver{file: pinnedRow},
			byPath:                   map[string]*models.MediaFile{ownerURI: ownerRow},
		},
	}
	key := virtualAdoptionRefusalKey(pinnedRow.ID, pinnedRow.FilePath)
	virtualAdoptionRefusals.clear(key)
	t.Cleanup(func() { virtualAdoptionRefusals.clear(key) })

	// Before the bound the duplicate-sibling guard refuses and returns no write.
	if row, _, ok := h.virtualProbeEvidenceRotateTarget(context.Background(), pinnedRow, ownerURI, probedDualAudioFile(), resolver.PersistedIdentityTiers{}); ok {
		t.Fatalf("duplicate same-release sibling rotated before the bound: %+v", row)
	}

	for i := 0; i < virtualAdoptionRefusalBound-1; i++ {
		virtualAdoptionRefusals.mark(key)
	}
	rotated, evidence, ok := h.virtualProbeEvidenceRotateTarget(context.Background(), pinnedRow, ownerURI, probedDualAudioFile(), resolver.PersistedIdentityTiers{})
	if !ok {
		t.Fatalf("bounded rotation still refused: consecutive refusals = %d", virtualAdoptionRefusals.count(key))
	}
	if rotated == nil || rotated.ID != ownerRow.ID {
		t.Fatalf("bounded rotation targeted %+v, want owner row %d", rotated, ownerRow.ID)
	}
	if evidence == nil || evidence.ID != ownerRow.ID {
		t.Fatalf("bounded rotation evidence targeted %+v, want owner row %d", evidence, ownerRow.ID)
	}
}

// TestRefusedProbeInventoryNeverReportsVerified proves the session-status half:
// a probe whose catalog write was refused is served in memory, but the session
// must not report it as verified. Verified means committed evidence (a row
// stamp) or an adopted identity, neither of which a refused write produced.
func TestRefusedProbeInventoryNeverReportsVerified(t *testing.T) {
	candidateURI := "virtual://series/tt39087958/1/1?result=9d8e30b32858eab16eb4e1b5"
	row := &models.MediaFile{
		ID: 5031897, ContentID: "episode-tvdb-471090-1-1", FilePath: candidateURI,
		// No ProbeUpdatedAt: the row holds a NULL stamp.
	}
	probed := &models.MediaFile{
		AudioTracks:    []models.AudioTrack{{Codec: "aac", Channels: 2}},
		SubtitleTracks: []models.SubtitleTrack{{Index: 1, Codec: "subrip", Language: "ko", Forced: true}},
	}
	h := NewPlaybackHandler(playback.NewSessionManager(0, 0), mapPlaybackFileResolver{
		files: map[int]*models.MediaFile{row.ID: row},
	})
	session := &playback.Session{ID: "s-refused", MediaFileID: row.ID, VirtualSourceURI: candidateURI}

	override := h.refusedProbeInventoryFile(context.Background(), session, row.ID, candidateURI, probed)
	if override == nil {
		t.Fatal("refused evidence was not served in memory at all")
	}
	if override.ProbeUpdatedAt != nil {
		t.Fatalf("refused evidence fabricated a probe stamp %v; a session must not report unpersisted evidence as verified", override.ProbeUpdatedAt)
	}

	inventory, err := h.playbackInventoryForFileV3(context.Background(), session, nil, override, candidateURI)
	if err != nil {
		t.Fatalf("build inventory: %v", err)
	}
	if inventory.InventoryStatus == string(ProbeProvenanceVerified) {
		t.Fatalf("refused evidence reported inventory_status=%q", inventory.InventoryStatus)
	}
}

// TestRefusedProbeInventoryStampedBaseRowStaysDeclared covers the stamped base
// row the earlier test missed: a refused probe replaces the base row's tracks,
// so the row's old committed stamp no longer describes them. The override must
// drop that stamp and report declared, not verified.
func TestRefusedProbeInventoryStampedBaseRowStaysDeclared(t *testing.T) {
	candidateURI := "virtual://series/tt39087958/1/1?result=stamped-base"
	probedAt := time.Now()
	row := &models.MediaFile{
		ID: 5031930, ContentID: "episode-tvdb-471090-1-1", FilePath: candidateURI,
		// The base row already carries committed probe evidence.
		ProbeUpdatedAt: &probedAt,
	}
	probed := &models.MediaFile{
		AudioTracks:    []models.AudioTrack{{Codec: "aac", Channels: 2}},
		SubtitleTracks: []models.SubtitleTrack{{Index: 1, Codec: "subrip", Language: "ko", Forced: true}},
	}
	h := NewPlaybackHandler(playback.NewSessionManager(0, 0), mapPlaybackFileResolver{
		files: map[int]*models.MediaFile{row.ID: row},
	})
	session := &playback.Session{ID: "s-stamped", MediaFileID: row.ID, VirtualSourceURI: candidateURI}

	override := h.refusedProbeInventoryFile(context.Background(), session, row.ID, candidateURI, probed)
	if override == nil {
		t.Fatal("refused evidence was not served in memory at all")
	}
	if override.ProbeUpdatedAt != nil {
		t.Fatalf("refused override kept the base row's probe stamp %v", override.ProbeUpdatedAt)
	}
	if !override.RefusedProbeEvidence {
		t.Fatal("refused override is not marked as uncommitted evidence")
	}
	inventory, err := h.playbackInventoryForFileV3(context.Background(), session, nil, override, candidateURI)
	if err != nil {
		t.Fatalf("build inventory: %v", err)
	}
	if inventory.InventoryStatus != string(ProbeProvenanceDeclared) {
		t.Fatalf("stamped base row reported refused evidence as inventory_status=%q, want declared", inventory.InventoryStatus)
	}
}

// TestRefusedProbeInventoryVerifiedSessionStaysDeclared covers the other half:
// a session that previously recorded a verified probe outcome must not lend that
// verdict to refused foreign evidence for a different source. Verified means a
// durable write adopted this identity, which the refusal did not, so the override
// stays declared.
func TestRefusedProbeInventoryVerifiedSessionStaysDeclared(t *testing.T) {
	candidateURI := "virtual://series/tt39087958/1/1?result=verified-then-refused"
	row := &models.MediaFile{
		ID: 5031931, ContentID: "episode-tvdb-471090-1-1", FilePath: candidateURI,
		// No ProbeUpdatedAt: the refusal is the only evidence for this candidate.
	}
	probed := &models.MediaFile{
		AudioTracks:    []models.AudioTrack{{Codec: "eac3", Channels: 6}},
		SubtitleTracks: []models.SubtitleTrack{{Index: 1, Codec: "subrip", Language: "en"}},
	}
	h := NewPlaybackHandler(playback.NewSessionManager(0, 0), mapPlaybackFileResolver{
		files: map[int]*models.MediaFile{row.ID: row},
	})
	session := &playback.Session{
		ID: "s-verified", MediaFileID: row.ID, VirtualSourceURI: candidateURI,
		VirtualProbeOutcome: playback.VirtualProbeOutcomeVerified,
	}

	override := h.refusedProbeInventoryFile(context.Background(), session, row.ID, candidateURI, probed)
	if override == nil {
		t.Fatal("refused evidence was not served in memory at all")
	}
	inventory, err := h.playbackInventoryForFileV3(context.Background(), session, nil, override, candidateURI)
	if err != nil {
		t.Fatalf("build inventory: %v", err)
	}
	if inventory.InventoryStatus != string(ProbeProvenanceDeclared) {
		t.Fatalf("previously verified session reported refused foreign evidence as inventory_status=%q, want declared", inventory.InventoryStatus)
	}
}

// TestVirtualAdoptionRefusalBoundLeavesSameReleasePathAlone is the happy-path
// guard: a row that already owns the candidate's exact path is never gated on an
// adoption and never consults the refusal bound, so the ordinary same-release
// write is untouched.
func TestVirtualAdoptionRefusalBoundLeavesSameReleasePathAlone(t *testing.T) {
	uri := "virtual://series/tt39087958/1/1?profile=4K+Dolby+Vision&result=owned"
	row := &models.MediaFile{
		ID: 5031901, ContentID: "episode-tvdb-471090-1-1", EpisodeID: "episode-tvdb-471090-1-1",
		FilePath: uri, MediaFolderID: 42, VirtualOwnerInstallationID: 7,
		ProviderVideoHash: "owned-hash",
	}
	probed := &models.MediaFile{ProviderVideoHash: "owned-hash"}
	h := &PlaybackHandler{}
	// Even with the bound long past, the row's own candidate is metadata-only.
	for i := 0; i < virtualAdoptionRefusalBound*2; i++ {
		virtualAdoptionRefusals.mark(virtualAdoptionRefusalKey(row.ID, row.FilePath))
	}
	t.Cleanup(func() { virtualAdoptionRefusals.clear(virtualAdoptionRefusalKey(row.ID, row.FilePath)) })

	args, ok := h.virtualProbeEvidenceArgsForRow(context.Background(), row, uri, probed, resolver.PersistedIdentityTiers{}, true)
	if !ok {
		t.Fatal("same-path write was refused")
	}
	if args.RequireAdopt {
		t.Fatalf("same-path write was gated on an adoption: %+v", args)
	}
	if args.AdoptPath != "" {
		t.Fatalf("same-path write nominated an adoption: %q", args.AdoptPath)
	}
	if args.RefusalReason != "" {
		t.Fatalf("same-path write carried a refusal reason: %q", args.RefusalReason)
	}
}

// TestVirtualAdoptionRefusalTrackerCountsAndResets proves the counter itself:
// it accumulates consecutive refusals, resets on success, and expires an idle
// mark so a recovered row is not permanently stale.
func TestVirtualAdoptionRefusalTrackerCountsAndResets(t *testing.T) {
	now := time.Now()
	cache := &virtualAdoptionRefusalCache{marks: map[string]virtualAdoptionRefusalMark{}, now: func() time.Time { return now }}
	const key = "row\x00pin"
	if got := cache.count(key); got != 0 {
		t.Fatalf("empty cache count = %d, want 0", got)
	}
	for want := 1; want <= 3; want++ {
		if got := cache.mark(key); got != want {
			t.Fatalf("mark %d returned %d", want, got)
		}
	}
	if got := cache.count(key); got != 3 {
		t.Fatalf("count = %d, want 3", got)
	}
	cache.clear(key)
	if got := cache.count(key); got != 0 {
		t.Fatalf("cleared count = %d, want 0", got)
	}
	cache.mark(key)
	now = now.Add(virtualAdoptionRefusalTTL + time.Second)
	if got := cache.count(key); got != 0 {
		t.Fatalf("expired count = %d, want 0", got)
	}
}

// TestVirtualAdoptionRefusalTrackerEnforcesMaxEntries proves the cap is a hard
// limit, not an expiry hint: inserting far more distinct live keys than the
// capacity must leave the map at or below capacity instead of growing past it.
// The pre-fix code pruned only expired marks and inserted regardless.
func TestVirtualAdoptionRefusalTrackerEnforcesMaxEntries(t *testing.T) {
	const capacity = 4
	now := time.Now()
	cache := &virtualAdoptionRefusalCache{
		marks:      map[string]virtualAdoptionRefusalMark{},
		now:        func() time.Time { return now },
		maxEntries: capacity,
	}
	for i := 0; i < capacity*10; i++ {
		cache.mark(strconv.Itoa(i) + "\x00pin")
	}
	cache.mu.Lock()
	live := len(cache.marks)
	cache.mu.Unlock()
	if live > capacity {
		t.Fatalf("live refusal marks = %d, want at most %d", live, capacity)
	}
	if live == 0 {
		t.Fatal("every mark was evicted; the most recent refusal was lost")
	}
	// The most recently marked key must still be present and counted.
	last := strconv.Itoa(capacity*10-1) + "\x00pin"
	if got := cache.count(last); got != 1 {
		t.Fatalf("most recent key count = %d, want 1", got)
	}
}

// TestVirtualAdoptionTerminalRefusalPersistsMarker was removed with the
// pre-emptive terminal decision: the terminal is now installed by the
// transactional refusal reconciliation (see txVirtualAdoptionRefusal), and its
// durable, revision-fenced stamp is exercised against the real schema in
// playback_virtual_adoption_bound_db_test.go.

// adoptionBoundRefusingDB is a fake VirtualFileMetadataDB that makes every
// required adoption match no row, so ExecVirtualFileMetadataUpdateResult takes
// its refusal path without a database.
type adoptionBoundRefusingDB struct{}

func (adoptionBoundRefusingDB) QueryRow(context.Context, string, ...any) pgx.Row {
	return adoptionBoundNoRow{}
}

type adoptionBoundNoRow struct{}

func (adoptionBoundNoRow) Scan(...any) error { return pgx.ErrNoRows }

// adoptionBoundAdoptingDB is a fake VirtualFileMetadataDB that reports the
// requested adopt path as persisted.
type adoptionBoundAdoptingDB struct{ path string }

func (d adoptionBoundAdoptingDB) QueryRow(context.Context, string, ...any) pgx.Row {
	return adoptionBoundPathRow(d)
}

type adoptionBoundPathRow struct{ path string }

func (r adoptionBoundPathRow) Scan(dest ...any) error {
	if len(dest) > 0 {
		if out, ok := dest[0].(*string); ok {
			*out = r.path
		}
	}
	return nil
}

// TestExecVirtualFileMetadataUpdateCountsAdoptionRefusals proves the saver half
// of the bound: the shared SQL executor counts each refused required adoption for
// the row and pin, and clears the count when a later write lands. Without the
// count a caller cannot tell a transient miss from a stale pin.
func TestExecVirtualFileMetadataUpdateCountsAdoptionRefusals(t *testing.T) {
	row := adoptionBoundTestRow(5031902, "count-hash")
	adopt := "virtual://series/tt39087958/1/1?profile=4K+Dolby+Vision&result=counted"
	args := models.VirtualFilePersistArgs{
		FileID:           row.ID,
		ExpectedFilePath: row.FilePath,
		AdoptPath:        adopt,
		RequireAdopt:     true,
	}
	key := virtualAdoptionRefusalKeyForArgs(args)
	virtualAdoptionRefusals.clear(key)
	t.Cleanup(func() { virtualAdoptionRefusals.clear(key) })

	for want := 1; want <= virtualAdoptionRefusalBound; want++ {
		if _, err := ExecVirtualFileMetadataUpdateResult(context.Background(), adoptionBoundRefusingDB{}, args); err == nil {
			t.Fatal("refused adoption reported success")
		}
		if got := virtualAdoptionRefusals.count(key); got != want {
			t.Fatalf("refusal count = %d, want %d", got, want)
		}
	}

	// A write that lands the requested identity resets the bound.
	result, err := ExecVirtualFileMetadataUpdateResult(context.Background(), adoptionBoundAdoptingDB{path: adopt}, args)
	if err != nil {
		t.Fatalf("adopting write failed: %v", err)
	}
	if !result.IdentityAdopted {
		t.Fatalf("adopting write did not report adoption: %+v", result)
	}
	if got := virtualAdoptionRefusals.count(key); got != 0 {
		t.Fatalf("refusal count after success = %d, want 0", got)
	}
}

// TestVirtualAdoptionRefusalKeyDistinguishesPins proves the bound is scoped to
// one row and pin: a rotation that moves the path starts a fresh count, and two
// rows never share one.
func TestVirtualAdoptionRefusalKeyDistinguishesPins(t *testing.T) {
	base := virtualAdoptionRefusalKey(1, "virtual://x?result=a")
	if base == "" {
		t.Fatal("key was empty")
	}
	if base == virtualAdoptionRefusalKey(1, "virtual://x?result=b") {
		t.Fatal("two pins of one row collapsed to one refusal key")
	}
	if base == virtualAdoptionRefusalKey(2, "virtual://x?result=a") {
		t.Fatal("two rows collapsed to one refusal key")
	}
	if virtualAdoptionRefusalKey(0, "virtual://x?result=a") != "" {
		t.Fatal("a zero row id produced a refusal key")
	}
}

// TestAdoptionBoundForeignTerminalLogIsActionable proves the terminal is
// observable: a foreign candidate past the bound makes the transactional
// refusal reconciliation name the exhausted disposition, so an operator sees a
// terminal rather than a transient miss.
func TestAdoptionBoundForeignTerminalLogIsActionable(t *testing.T) {
	logs := captureHandlerLogs(t)
	row := adoptionBoundTestRow(5031900, "row-hash")
	candidate := "virtual://series/tt39087958/1/1?profile=4K+Dolby+Vision&result=foreign"
	args := txAdoptionArgs(row.ID, row.FilePath, candidate)
	tx := &fakeAdoptionTx{rowFor: func(sql string, _ ...any) pgx.Row {
		if sql == lockVirtualFileRowSQL {
			return scriptedRow{id: row.ID}
		}
		return scriptedRow{err: pgx.ErrNoRows}
	}}
	db := &fakeTxBeginner{tx: tx}
	store := newFakeAdoptionTxStore()
	// One short of the bound, so this refused adoption reaches it and names the
	// terminal from the in-transaction count.
	store.counts[store.key(row.ID, row.FilePath)] = virtualAdoptionRefusalBound - 1
	store.terminalStamped = true
	if _, err := ExecVirtualFileMetadataUpdateResultWithRefusals(context.Background(), db, args, store); err == nil {
		t.Fatal("refused adoption reported success")
	}
	if !strings.Contains(logs.String(), virtualProbeRefusalAdoptionBoundExhausted) {
		t.Fatalf("terminal refusal reason not logged:\n%s", logs.String())
	}
}

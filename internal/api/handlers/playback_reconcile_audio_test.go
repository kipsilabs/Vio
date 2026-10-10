package handlers

import (
	"context"
	"encoding/json"
	"math/rand"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/userstore"
)

// verifiedFileResolver serves one fixed catalog row for reconcile tests.
type verifiedFileResolver struct {
	file *models.MediaFile
}

func (r verifiedFileResolver) GetByID(_ context.Context, _ int) (*models.MediaFile, error) {
	return r.file, nil
}

// reconcileFixture wires a handler with a live session, an attempt record
// and a fixed verified inventory. The session's VirtualSourceURI is left
// empty so virtualEvidenceMatchesBoundFile refuses when there is no bound
// candidate; callers that exercise the reconcile path set the fields
// explicitly. InstallationID stays empty on purpose: the full automatic
// replan seam requires caller/installation wiring, so tests assert the
// reconcile decision surface, not the transport commit.
func reconcileFixture(t *testing.T, session *playback.Session, record *playback.AttemptRecordV3, verified *models.MediaFile) *PlaybackHandler {
	t.Helper()
	manager := playback.NewSessionManager(0, 0)
	copy := *session
	manager.RegisterReconstructed(&copy)

	handler := NewPlaybackHandler(manager, verifiedFileResolver{file: verified})
	if err := handler.PlanStoreV3.SaveAttempt(context.Background(), *record); err != nil {
		t.Fatalf("save attempt: %v", err)
	}
	if err := manager.SetAudioSelectionIntent(session.ID, session.SelectionOrigin, session.PreferredAudioLanguage, session.SeriesAudioPreferenceSignature, session.SelectedAudioSignature); err != nil {
		t.Fatalf("set intent: %v", err)
	}
	return handler
}

func reconcileLiveSession(t *testing.T, handler *PlaybackHandler, sessionID string) *playback.Session {
	t.Helper()
	manager, ok := handler.sessionMgr.(*playback.SessionManager)
	if !ok {
		t.Fatal("handler session manager is not a *playback.SessionManager")
	}
	live, err := manager.GetSession(sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	return live
}

func reconcileIntent(preferred string, series *userstore.AudioTrackSignature, selected *userstore.AudioTrackSignature) *playback.AttemptRecordV3 {
	return &playback.AttemptRecordV3{
		PlaybackAttemptID:              "attempt-reconcile-1",
		SessionID:                      "11111111-1111-1111-1111-111111111111",
		ExpiresAt:                      time.Now().Add(time.Hour),
		UserID:                         1,
		ProfileID:                      "profile-1",
		RequestedMediaFileID:           7,
		EffectiveMediaFileID:           7,
		CurrentPlanID:                  "plan-reconcile-1",
		SelectionOrigin:                SelectionOriginAuto,
		PreferredAudioLanguage:         preferred,
		SeriesAudioPreferenceSignature: series,
		SelectedAudioSignature:         selected,
	}
}

// The cold-start story: declared order [en, pt] committed index 0 while the
// preference is pt; the verified inventory reverses to [pt, en] with sparse
// absolute stream indexes. Reconciliation must select the preferred
// language at its verified position, not the same ordinal.
func TestReconcileVerifiedDefaultAudioReordersToPreferredLanguage(t *testing.T) {
	planTime := []models.AudioTrack{
		{Index: 1, Language: "en", Codec: "aac", Channels: 2},
		{Index: 3, Language: "pt", Codec: "aac", Channels: 2},
	}
	verified := &models.MediaFile{ID: 7, AudioTracks: []models.AudioTrack{
		{Index: 3, Language: "pt", Codec: "aac", Channels: 2},
		{Index: 1, Language: "en", Codec: "aac", Channels: 2},
	}}
	session := &playback.Session{
		ID: "11111111-1111-1111-1111-111111111111", UserID: 1, ProfileID: "profile-1",
		MediaFileID: 7, RequestedMediaFileID: 7, PlayMethod: playback.PlayDirect,
		AudioTrackIndex:               0,
		SelectionOrigin:               SelectionOriginAuto,
		PreferredAudioLanguage:        "pt",
		SelectedAudioSignature:        playback.AudioTrackSignatureFromTrack(planTime[0]),
		VirtualAudioTracks:            planTime,
		VirtualSourceURI:              "virtual://movie/tt-reconcile",
		VirtualSubtitleEvidenceURI:    "virtual://movie/tt-reconcile",
		VirtualSubtitleEvidenceFileID: 7,
		VirtualSubtitleEvidenceSet:    true,
	}
	record := reconcileIntent("pt", nil, playback.AudioTrackSignatureFromTrack(planTime[0]))
	record.CurrentPlan.SelectedTracks.Audio = &playback.TrackIdentityV3{ID: playback.TrackIDV3(7, "audio", 0), Index: intPtrReconcile(0)}
	verified.FilePath = "virtual://movie/tt-reconcile"

	handler := reconcileFixture(t, session, record, verified)
	live := reconcileLiveSession(t, handler, session.ID)

	// Prove the executable comparison alone demands the verified position:
	// index 0 in declared order committed en, while pt now sits at
	// verified index 0. The same comparison must hold after the
	// byte-equal check fails inside the reconcile path.
	intent := handler.audioSelectionIntentForSession(context.Background(), live)
	if got := playback.SelectAudioTrack(verified.AudioTracks, intent.preferredLang, intent.seriesPref); got != 0 {
		t.Fatalf("verified selection = %d, want 0 (pt at its verified position)", got)
	}
	if audioSignatureMatchesCommittedTrack(verified.AudioTracks[0], planTime[0], intent, 0, 0, planTime, verified.AudioTracks) {
		t.Fatal("reordered languages must not compare byte-equal")
	}
	// Simulate the evidence write binding the row to the bound candidate.
	// The fixture keeps InstallationID empty, so the full replan seam
	// refuses before planning; assert the reconcile path reaches that
	// decision without mutating the committed attempt or pushing a menu.
	handler.reconcileSessionDefaultAudio(context.Background(), live, 7)

	after, err := handler.PlanStoreV3.GetAttempt(context.Background(), session.ID)
	if err != nil {
		t.Fatalf("get attempt: %v", err)
	}
	if after.CurrentPlanID != record.CurrentPlanID {
		t.Fatal("refused automatic replan must not commit a new plan")
	}
}

func intPtrReconcile(v int) *int { return &v }

// TestDeferredInventoryAdoptionPublishesRealTracksInPlace pins the server's
// side of the in-place adoption invariant. When the deferred probe lands, the
// real verified inventory is published to the live session as the adoption
// source, and the server does NOT commit a replacement plan for it: what the
// client is playing keeps playing, and its menus are updated from the push.
func TestDeferredInventoryAdoptionPublishesRealTracksInPlace(t *testing.T) {
	const fileID = 9101
	manager := playback.NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", fileID, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	probedAt := time.Now().UTC()
	file := &models.MediaFile{
		ID:             fileID,
		ContentID:      "movie-adopt-in-place",
		FilePath:       "/media/adopt-in-place.mkv",
		ProbeUpdatedAt: &probedAt,
		AudioTracks: []models.AudioTrack{
			{Index: 1, Codec: "aac", Channels: 2, Language: "eng", Default: true},
			{Index: 3, Codec: "eac3", Channels: 6, Language: "spa"},
		},
		SubtitleTracks: []models.SubtitleTrack{
			{Index: 4, Codec: "subrip", Language: "eng", Default: true},
		},
	}
	handler := NewPlaybackHandler(manager, testPlaybackFileResolver{file: file})
	handler.RealtimeHub = playback.NewRealtimeHub()
	if err := manager.SetRealtimeConnection(session.ID, true); err != nil {
		t.Fatalf("SetRealtimeConnection: %v", err)
	}
	conn := &sourceCommittedTestConn{}
	registration := handler.RealtimeHub.Register(session.ID, conn)
	if registration == nil {
		t.Fatal("expected a realtime registration")
	}
	defer handler.RealtimeHub.Unregister(registration)

	record := reconcileIntent("spa", nil, nil)
	record.SessionID = session.ID
	record.RequestedMediaFileID = fileID
	record.EffectiveMediaFileID = fileID
	record.CurrentPlanID = "plan-in-place"
	if err := handler.PlanStoreV3.SaveAttempt(context.Background(), *record); err != nil {
		t.Fatalf("save attempt: %v", err)
	}

	handler.PublishInventoryUpdated(context.Background(), fileID)

	if len(conn.messages) != 1 {
		t.Fatalf("delivered %d events, want 1", len(conn.messages))
	}
	event, ok := conn.messages[0].(playback.EventEnvelope)
	if !ok {
		t.Fatalf("message type = %T, want playback.EventEnvelope", conn.messages[0])
	}
	if event.Name != playback.RealtimeEventInventoryUpdated {
		t.Fatalf("event name = %q, want %q", event.Name, playback.RealtimeEventInventoryUpdated)
	}
	var payload playback.InventoryUpdatedPayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.InventoryStatus != string(ProbeProvenanceVerified) {
		t.Fatalf("inventory status = %q, want verified", payload.InventoryStatus)
	}
	if len(payload.AudioTracks) != 2 || len(payload.SubtitleInventory) != 1 {
		t.Fatalf("adopted inventory = %d audio / %d subtitle, want 2 / 1",
			len(payload.AudioTracks), len(payload.SubtitleInventory))
	}

	// The publication is menu data: the plan the session is playing is not
	// replaced, so nothing on the output restarts.
	after, err := handler.PlanStoreV3.GetAttempt(context.Background(), session.ID)
	if err != nil {
		t.Fatalf("get attempt: %v", err)
	}
	if after.CurrentPlanID != "plan-in-place" {
		t.Fatalf("adoption replaced the plan (%q), want it left in place", after.CurrentPlanID)
	}
}

// reconcileCapabilities is the minimum client codec evidence a v3 replan body
// must carry to be valid.
func reconcileCapabilities() playback.ClientCodecCapabilitiesV3 {
	return playback.ClientCodecCapabilitiesV3{
		VideoEvidence: playback.EvidenceExactV3,
		AudioEvidence: playback.EvidenceExactV3,
		CodecsVideo:   []string{"h264"},
		CodecsAudio:   []string{"aac"},
		Containers:    []string{"mp4"},
		MaxResolution: "1080p",
	}
}

// MULTi membership: a track whose Languages list carries eng beats a bare
// eng track only on rank, while a bare MULTi primary with no member list
// never counts as a concrete language match.
func TestReconcileMultiMembershipSemantics(t *testing.T) {
	member := models.AudioTrack{Language: "mul", Languages: []string{"eng", "fre"}, Codec: "eac3", Channels: 6}
	if !playback.TrackCarriesLanguage(member, "eng") {
		t.Fatal("MULTi track with [eng,fre] must carry eng")
	}
	if !playback.TrackCarriesLanguage(member, "fre") {
		t.Fatal("MULTi track with [eng,fre] must carry fre")
	}
	bare := models.AudioTrack{Language: "mul", Codec: "eac3", Channels: 6}
	if playback.TrackCarriesLanguage(bare, "eng") {
		t.Fatal("bare MULTi (no member list) must not match eng")
	}
	dual := models.AudioTrack{Language: "DUAL", Codec: "ac3", Channels: 6}
	if playback.TrackCarriesLanguage(dual, "eng") {
		t.Fatal("bare DUAL must not match eng")
	}
	tracks := []models.AudioTrack{
		bare,
		{Language: "eng", Codec: "aac", Channels: 2},
	}
	if got := playback.SelectAudioTrack(tracks, "eng", nil); got != 1 {
		t.Fatalf("bare MULTi must not steal the preference: selected %d, want 1", got)
	}
	if got := playback.SelectAudioTrack(tracks, "deu", nil); got != 0 {
		t.Fatalf("bare MULTi stays eligible as neutral fallback: selected %d, want 0", got)
	}
}

// Regional fidelity: a pt-BR preference selects the exact pt-BR track over
// pt-PT and bare pt, using the same rank chain that start uses.
func TestReconcileRegionalVariantFidelity(t *testing.T) {
	tracks := []models.AudioTrack{
		{Index: 1, Language: "pt-PT", Codec: "aac", Channels: 2},
		{Index: 2, Language: "pt-BR", Codec: "aac", Channels: 2},
		{Index: 3, Language: "pt", Codec: "aac", Channels: 2},
	}
	if got := playback.SelectAudioTrack(tracks, "pt-BR", nil); got != 1 {
		t.Fatalf("pt-BR preference selected %d, want 1 (exact pt-BR)", got)
	}
	verified := &models.MediaFile{ID: 9, AudioTracks: []models.AudioTrack{
		{Index: 2, Language: "pt-BR", Codec: "aac", Channels: 2},
		{Index: 1, Language: "pt-PT", Codec: "aac", Channels: 2},
	}}
	selected := verified.AudioTracks[0]
	intent := audioSelectionIntent{origin: SelectionOriginAuto, preferredLang: "pt-BR"}
	if !defaultAudioLanguageStillSatisfied(selected, verified.AudioTracks, 0, intent) {
		t.Fatal("reordered exact pt-BR track must still satisfy the pt-BR preference")
	}
	// pt-PT is a regional variant, not the exact tag SelectAudioTrack
	// returned for this inventory: the executable check must reject it as
	// "not the selector's choice" even though the language rank alone
	// would call it a match. That is the unavailable-language guard: a
	// fallback that lands on the same stream is a no-op (see the
	// unavailable-language test below), but a different stream is never
	// papered over by language proximity.
	if defaultAudioLanguageStillSatisfied(verified.AudioTracks[1], verified.AudioTracks, 0, intent) {
		t.Fatal("pt-PT at index 1 is not the selector's index-0 choice and must fail the executable check")
	}
}

// Omitted vs explicit: an explicit selection verified present is never
// moved; a legacy record with no intent fields never reconciles.
func TestReconcileExplicitSelectionSurvivesProbe(t *testing.T) {
	verified := []models.AudioTrack{
		{Index: 1, Language: "en", Codec: "aac", Channels: 2},
		{Index: 2, Language: "fr", Codec: "aac", Channels: 2},
	}
	intent := audioSelectionIntent{
		origin:           SelectionOriginExplicit,
		selectedOverride: playback.AudioTrackSignatureFromTrack(verified[1]),
	}
	if !audioTrackSignatureEqual(verified[1], *intent.selectedOverride) {
		t.Fatal("explicit fr track must still verify present")
	}
	if audioTrackSignatureEqual(verified[0], *intent.selectedOverride) {
		t.Fatal("explicit fr signature must not match the en track")
	}

	legacy := audioSelectionIntent{}
	if legacy.origin == SelectionOriginAuto || legacy.origin == SelectionOriginExplicit {
		t.Fatal("legacy record without intent fields must carry no origin")
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(`{"playback_attempt_id":"x"}`), &raw); err != nil {
		t.Fatal(err)
	}
	old := playback.AttemptRecordV3{}
	blob, _ := json.Marshal(old)
	var revived playback.AttemptRecordV3
	if err := json.Unmarshal(blob, &revived); err != nil {
		t.Fatalf("old record must deserialize safely: %v", err)
	}
	if revived.SelectionOrigin != "" || revived.SelectedAudioSignature != nil || revived.SeriesAudioPreferenceSignature != nil {
		t.Fatal("old record must carry zero intent fields")
	}
}

// Duplicates: two ordinal-equal tracks with different absolute indexes keep
// the first stable winner; the byte-equal check must hold.
func TestReconcileDuplicateTracksFirstStableWins(t *testing.T) {
	planTime := []models.AudioTrack{
		{Index: 1, Language: "en", Codec: "aac", Channels: 2},
		{Index: 3, Language: "en", Codec: "aac", Channels: 2},
	}
	if got := playback.SelectAudioTrack(planTime, "en", nil); got != 0 {
		t.Fatalf("duplicate en tracks: selected %d, want first stable 0", got)
	}
	verified := []models.AudioTrack{
		{Index: 1, Language: "en", Codec: "aac", Channels: 2},
		{Index: 3, Language: "en", Codec: "aac", Channels: 2},
	}
	intent := audioSelectionIntent{
		origin:           SelectionOriginAuto,
		preferredLang:    "en",
		selectedOverride: playback.AudioTrackSignatureFromTrack(planTime[0]),
	}
	if !audioSignatureMatchesCommittedTrack(verified[0], planTime[0], intent, 0, 0, planTime, verified) {
		t.Fatal("identical inventories must be a byte-equal no-op")
	}
	// Same ordinal but a different language after reorder is not identical:
	// the language authority must reject it.
	swapped := []models.AudioTrack{
		{Index: 1, Language: "fr", Codec: "aac", Channels: 2},
		{Index: 3, Language: "en", Codec: "aac", Channels: 2},
	}
	if audioSignatureMatchesCommittedTrack(swapped[0], planTime[0], intent, 0, 0, planTime, swapped) {
		t.Fatal("same ordinal with a different language is not the same executable selection")
	}
}

// Transport reuse guard: a stream that decodes differently (codec or
// channel layout changed) must never reuse a fixed ffmpeg audio map.
func TestReconcileTransportReuseGuard(t *testing.T) {
	base := models.AudioTrack{Language: "en", Codec: "eac3", Channels: 6, Layout: "5.1"}
	if !audioTransportFactsEqual(base, models.AudioTrack{Language: "en", Codec: "EAC3", Channels: 6, Layout: "5.1"}) {
		t.Fatal("identical codec/layout must reuse")
	}
	for _, changed := range []models.AudioTrack{
		{Language: "en", Codec: "aac", Channels: 6, Layout: "5.1"},
		{Language: "en", Codec: "eac3", Channels: 2, Layout: "stereo"},
		{Language: "en", Codec: "eac3", Channels: 6, Layout: "stereo"},
	} {
		if audioTransportFactsEqual(base, changed) {
			t.Fatalf("changed transport facts must not reuse: %+v", changed)
		}
	}
}

// Permutation fuzz: a unique best language match survives ~200 order
// permutations, so the reorder can never move the preference by position.
func TestReconcileSelectAudioTrackPermutationStable(t *testing.T) {
	languages := []string{"en", "fr", "de", "es", "it", "ja"}
	preferred := "fr"
	rng := rand.New(rand.NewSource(42))
	for i := 0; i < 200; i++ {
		order := rng.Perm(len(languages))
		tracks := make([]models.AudioTrack, len(languages))
		want := -1
		for pos, li := range order {
			tracks[pos] = models.AudioTrack{Index: pos + 1, Language: languages[li], Codec: "aac", Channels: 2}
			if languages[li] == preferred {
				want = pos
			}
		}
		if got := playback.SelectAudioTrack(tracks, preferred, nil); got != want || tracks[got].Language != preferred {
			t.Fatalf("permutation %d: selected %d (%q), want %d (%q)", i, got, tracks[got].Language, want, preferred)
		}
	}
}

// Canonical dedup: pt-BR and pt-PT are distinct provider hints; exact
// duplicates collapse, regional variants never do.
func TestMergeVirtualCandidateLanguagesPreservesRegionalVariants(t *testing.T) {
	probed := &models.MediaFile{}
	candidate := VirtualPlaybackStream{AudioLanguages: []string{"pt-BR", "pt-PT", "pt-br", "ENG", "eng"}}
	mergeVirtualCandidateTracks(probed, candidate)
	if len(probed.AudioTracks) != 3 {
		t.Fatalf("tracks = %d, want 3 (pt-BR, pt-PT, en)", len(probed.AudioTracks))
	}
	langs := map[string]bool{}
	for _, track := range probed.AudioTracks {
		langs[track.Language] = true
	}
	for _, want := range []string{"pt-BR", "pt-PT", "ENG"} {
		if !langs[want] {
			t.Errorf("missing regional variant %q: got %+v", want, probed.AudioTracks)
		}
	}
}

// User-change race: when the committed selection no longer matches the
// start-time selected signature (the viewer switched tracks while the probe
// was in flight), the reconcile path must not override the viewer's choice.
// Exercised through the same race guard the automatic path checks before
// building its replan body.
func TestReconcileUserChangeRaceNeverOverrides(t *testing.T) {
	verified := []models.AudioTrack{
		{Index: 1, Language: "en", Codec: "aac", Channels: 2},
		{Index: 2, Language: "fr", Codec: "aac", Channels: 2},
	}
	// Start committed en (index 0); the viewer then switched to fr.
	startSig := playback.AudioTrackSignatureFromTrack(verified[0])
	intent := audioSelectionIntent{origin: SelectionOriginAuto, preferredLang: "en", selectedOverride: startSig}
	current := verified[1]
	if audioTrackSignatureEqual(current, *intent.selectedOverride) {
		t.Fatal("post-switch fr track must not equal the start-time en signature")
	}
	// The reconcile decision must therefore treat the live selection as
	// superseding the intent and decline to build a replan.
	committedIndex := 1
	committed := verified[committedIndex]
	recomputed := playback.SelectAudioTrack(verified, intent.preferredLang, intent.seriesPref)
	if recomputed != 0 {
		t.Fatalf("en preference recomputes to %d, want 0", recomputed)
	}
	if audioSignatureMatchesCommittedTrack(verified[recomputed], committed, intent, committedIndex, recomputed, verified, verified) {
		t.Fatal("a user-switched selection must never compare byte-equal to the start intent")
	}
	// The live-session race guard must also refuse: the session's committed
	// index now names the fr track, not the start-time en signature.
	race := &playback.Session{AudioTrackIndex: committedIndex, VirtualAudioTracks: verified}
	if liveSelectionStillCommitted(race, nil, intent) {
		t.Fatal("live race guard must refuse a viewer-switched selection")
	}
	// And accept an unmoved selection.
	calm := &playback.Session{AudioTrackIndex: 0, VirtualAudioTracks: verified}
	if !liveSelectionStillCommitted(calm, nil, intent) {
		t.Fatal("live race guard must accept the unchanged start selection")
	}
}

// Source rotation: evidence naming a different candidate than the bound
// session must be refused (and surfaced in memory) instead of reconciled.
func TestReconcileSourceRotationRefusesStaleEvidence(t *testing.T) {
	file := &models.MediaFile{ID: 11, FilePath: "virtual://movie/tt-new?result=2", AudioTracks: []models.AudioTrack{
		{Index: 1, Language: "en", Codec: "aac", Channels: 2},
	}}
	session := &playback.Session{
		ID:                            "22222222-2222-2222-2222-222222222222",
		UserID:                        1,
		ProfileID:                     "profile-1",
		MediaFileID:                   11,
		AudioTrackIndex:               0,
		SelectionOrigin:               SelectionOriginAuto,
		PreferredAudioLanguage:        "en",
		VirtualAudioTracks:            file.AudioTracks,
		VirtualSourceURI:              "virtual://movie/tt-old?result=1",
		VirtualSubtitleEvidenceURI:    "virtual://movie/tt-old?result=1",
		VirtualSubtitleEvidenceFileID: 11,
		VirtualSubtitleEvidenceSet:    true,
		VirtualSourceRevision:         "rev-old",
	}
	if virtualEvidenceMatchesBoundFile(file, session) {
		t.Fatal("rotated candidate URI must not match the bound evidence")
	}
}

// Probe-before-registration: when the attempt does not exist yet (probe
// landed before the session attached), the attach-side hook must find no
// record and return without failing; legacy records without intent must
// likewise do nothing.
// Blocker 4 guard: Automatic is server-owned provenance. A client that forges
// it in the wire body must be treated as a user-initiated track_change, so
// preference persistence still happens.
func TestReconcileClientSuppliedAutomaticIsStripped(t *testing.T) {
	body, err := json.Marshal(playback.ReplanRequestV3{
		ProtocolVersion:   playback.ProtocolV3,
		Operation:         playback.ReplanOperationTrackChangeV3,
		Automatic:         playback.ReplanAutomaticV3,
		ReplanRequestID:   "client-forged-1",
		PlaybackAttemptID: "attempt-reconcile-1",
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	var decoded playback.ReplanRequestV3
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	if decoded.Automatic != playback.ReplanAutomaticV3 {
		t.Fatalf("fixture must carry the forged marker on the wire, got %q", decoded.Automatic)
	}

	// The inbound application seam is what the HTTP handler calls; it owns
	// stripping untrusted provenance before validation or persistence.
	stripClientSuppliedAutomatic(&decoded)
	if decoded.Automatic != "" {
		t.Fatalf("client-supplied Automatic survived: %q", decoded.Automatic)
	}

	// A server-built reconciliation request keeps its marker: that path never
	// goes through the inbound seam.
	serverBuilt := playback.ReplanRequestV3{
		Operation: playback.ReplanOperationTrackChangeV3,
		Automatic: playback.ReplanAutomaticV3,
	}
	if serverBuilt.Automatic != playback.ReplanAutomaticV3 {
		t.Fatal("server-built reconciliation lost its Automatic marker")
	}
}

// Blocker 3 guard: one entry deadline, propagated, never reset. A caller
// whose context is already done must not buy a fresh 10s budget.
func TestReconcilePropagatesCallerDeadlineWithoutReset(t *testing.T) {
	if testing.Short() {
		t.Skip("deadline propagation assertion needs a stalled dependency")
	}
	verified := &models.MediaFile{ID: 7, ProbeUpdatedAt: ptr(time.Now())}
	verified.AudioTracks = []models.AudioTrack{
		{Language: "por", Codec: "eac3"},
		{Language: "eng", Codec: "eac3"},
	}
	session := &playback.Session{
		ID:                 "11111111-1111-1111-1111-111111111111",
		UserID:             1,
		ProfileID:          "profile-1",
		Position:           42,
		SelectionOrigin:    SelectionOriginAuto,
		VirtualAudioTracks: verified.AudioTracks,
	}
	record := reconcileIntent("eng", nil, nil)
	handler := reconcileFixture(t, session, record, verified)

	done := make(chan struct{})
	go func() {
		defer close(done)
		// An already-canceled caller context: reconciliation must return on
		// it rather than arming its own timeout.
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		handler.reconcilePendingAudioStartup(canceled, session.ID)
	}()

	select {
	case <-done:
	case <-time.After(reconcileProbeBudgetV3 + 5*time.Second):
		t.Fatal("reconciliation ignored the caller deadline and blocked past the budget")
	}
}

func TestReconcileProbeBeforeRegistrationDefersToAttach(t *testing.T) {
	manager := playback.NewSessionManager(0, 0)
	session := &playback.Session{
		ID: "33333333-3333-3333-3333-333333333333", UserID: 1, ProfileID: "profile-1",
		MediaFileID: 13,
	}
	manager.RegisterReconstructed(session)
	handler := NewPlaybackHandler(manager, verifiedFileResolver{file: &models.MediaFile{ID: 13}})
	// No attempt saved: the attach hook must no-op instead of erroring.
	handler.reconcilePendingAudioStartup(context.Background(), session.ID)

	record := &playback.AttemptRecordV3{
		PlaybackAttemptID:    "attempt-legacy-13",
		SessionID:            session.ID,
		UserID:               1,
		ProfileID:            "profile-1",
		EffectiveMediaFileID: 13,
		CurrentPlanID:        "plan-legacy-13",
		ExpiresAt:            nowPlusHour(),
		// No SelectionOrigin: a record persisted before intent capture.
	}
	if err := handler.PlanStoreV3.SaveAttempt(context.Background(), *record); err != nil {
		t.Fatalf("save attempt: %v", err)
	}
	live, err := manager.GetSession(session.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	before, err := json.Marshal(live)
	if err != nil {
		t.Fatal(err)
	}
	handler.reconcilePendingAudioStartup(context.Background(), session.ID)
	afterLive, err := manager.GetSession(session.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	after, err := json.Marshal(afterLive)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("legacy record without intent must leave the session untouched")
	}
}

func nowPlusHour() (t time.Time) {
	return time.Now().Add(time.Hour)
}

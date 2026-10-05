package playback

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/Silo-Server/silo-server/internal/userstore"
)

var ErrIdempotencyKeyReusedV3 = errors.New("idempotency key reused")
var ErrPlaybackAttemptExistsV3 = errors.New("playback attempt already exists")
var ErrStaleReplanLeaseV3 = errors.New("stale replan lease")

// ErrReplanSupersededV3 means a CompleteReplan lost the revision compare: a
// newer replan already moved the attempt past the caller's base revision.
var ErrReplanSupersededV3 = errors.New("replan superseded")

// ErrRecoveryRevisionConflictV3 means an AppendRecoveryExclusions lost the
// recovery_revision compare: a concurrent writer already advanced the durable
// recovery state. The caller re-reads the returned state and retries the union
// against it; the append is monotone, so a retry converges.
var ErrRecoveryRevisionConflictV3 = errors.New("recovery revision conflict")

// ErrProgressConflictV3 means a progress sample reused an already-applied
// sequence number with a different payload: a retry must replay the exact
// sample it originally sent.
var ErrProgressConflictV3 = errors.New("progress sequence conflict")

// ErrAttemptStoppedV3 means the attempt has already been stopped, so no
// further progress can be applied to it.
var ErrAttemptStoppedV3 = errors.New("playback attempt stopped")

// ErrInvalidStopIDV3 means the stop id is not a UUID; the attempt row stores
// it as one so a replayed stop can be matched exactly.
var ErrInvalidStopIDV3 = errors.New("invalid stop id")

// ProgressSampleV3 is one client-reported playback position. Samples for an
// attempt are totally ordered by Sequence; a higher sequence always wins,
// even when its position moves backward.
type ProgressSampleV3 struct {
	Sequence int64   `json:"sequence"`
	Position float64 `json:"position"`
	IsPaused bool    `json:"is_paused"`
}

// Progress receipt outcomes.
const (
	// ProgressAppliedV3: the sample was newer than the row and is now the
	// latest accepted sample.
	ProgressAppliedV3 = "applied"
	// ProgressReplayedV3: the sample repeats the latest accepted one exactly
	// (same sequence and payload), so a retry after a lost reply is a no-op.
	ProgressReplayedV3 = "replayed"
	// ProgressStaleSampleV3: the row already holds a newer sequence; Accepted
	// carries that latest sample.
	ProgressStaleSampleV3 = "stale_sample"
)

// ProgressReceiptV3 is the durable outcome of one ApplyProgress call.
type ProgressReceiptV3 struct {
	Outcome string
	// Accepted is the latest sample the attempt holds after the call: the
	// caller's sample when applied or replayed, the newer stored sample when
	// stale, nil when nothing has been applied yet.
	Accepted *ProgressSampleV3
}

// StopReceiptV3 is what a stop returns, and what every later stop of the same
// attempt replays. It is stored on the attempt row as JSON.
type StopReceiptV3 struct {
	StopID string `json:"stop_id"`
	// Accepted is the final sample the stop settled on: the stop's own final
	// sample when it was newer than the row, otherwise the last applied
	// progress sample; nil when the attempt never reported progress.
	Accepted *ProgressSampleV3 `json:"accepted,omitempty"`
	// HistoryID is the watch-history row the stop writer produced, recorded
	// through RecordStopReceipt after the writer runs.
	HistoryID string `json:"history_id,omitempty"`
	// Finalized is set by RecordStopReceipt once the stop's side effects
	// (deny marker, teardown, history) have run. A replayed stop that finds it
	// unset finishes them: the winning replica died between the CAS and the
	// writers.
	Finalized bool `json:"finalized,omitempty"`
	// FinalizingUntil is the lease of the caller currently running the side
	// effects (see ProgressStoreV3.ClaimStopFinalization). Zero when nobody
	// holds the claim.
	FinalizingUntil time.Time `json:"finalizing_until,omitempty"`
}

// ProgressStoreV3 is the durable per-attempt progress and stop sequencing.
// Every method keys on the session id of a live (unexpired) attempt row and
// returns ErrSessionNotFound when there is none.
type ProgressStoreV3 interface {
	// ApplyProgress is a compare-and-set on the attempt's last_sequence:
	// applied when the sample is newer, replayed when it repeats the latest
	// sample exactly, stale_sample when the row is already past it, and
	// ErrProgressConflictV3 when the same sequence carries a different
	// payload. A stopped attempt returns ErrAttemptStoppedV3.
	ApplyProgress(ctx context.Context, sessionID string, sample ProgressSampleV3) (ProgressReceiptV3, error)
	// StopAttempt is a compare-and-set on stopped_at. The first stop wins: it
	// records stopID, applies the optional final sample when newer, and
	// returns first=true with the accepted sample. Every later call, with any
	// stop id, returns the stored receipt with first=false.
	StopAttempt(ctx context.Context, sessionID, stopID string, final *ProgressSampleV3) (StopReceiptV3, bool, error)
	// RecordStopReceipt stores the completed receipt (history id, accepted
	// sample) on a stopped row after the stop writer has run, so replayed
	// stops return what the first one produced.
	RecordStopReceipt(ctx context.Context, sessionID string, receipt StopReceiptV3) error
	// ClaimStopFinalization is a compare-and-set on the receipt's Finalizing
	// flag. Exactly one caller wins the claim on a stopped, unfinalized row
	// and runs the non-idempotent writers (history, scrobbles); every other
	// caller returns false and replays the stored receipt as is. A claim is
	// released by RecordStopReceipt with Finalized set, or by a later
	// ClaimStopFinalization once the claim's lease has passed.
	ClaimStopFinalization(ctx context.Context, sessionID string, leaseUntil time.Time) (bool, error)
}

type AttemptRecordV3 struct {
	PlaybackAttemptID      string
	SessionID              string
	UserID                 int
	ProfileID              string
	RequestedMediaFileID   int
	EffectiveMediaFileID   int
	CurrentPlanID          string
	CurrentReplanRequestID string
	CurrentPlan            PlanV3
	FrozenRecipe           ExecutableRecipeV3
	NormalizedRequest      StartRequestV3
	// ServerBitrateCapKbps is fixed when the attempt starts; replans must not
	// pick up later administrator edits to the account or access group.
	ServerBitrateCapKbps int
	// SelectionOrigin records whether the committed audio selection came
	// from the server's language preference ("auto") or an explicit viewer
	// choice ("explicit"). Legacy records predate the field and carry the
	// empty string; reconciliation must then do nothing, never assume a
	// default, since the pre-intent selection semantics are unknown.
	SelectionOrigin string `json:"selection_origin,omitempty"`
	// PreferredAudioLanguage is the canonical full tag that steered the
	// server-preference selection (e.g. "pt-BR"), or "" when none was
	// resolved. Compared against the verified inventory, never against an
	// ordinal.
	PreferredAudioLanguage string `json:"preferred_audio_language,omitempty"`
	// SeriesAudioPreferenceSignature is the series-preference snapshot the
	// start fed to SelectAudioTrack. Old records leave it nil.
	SeriesAudioPreferenceSignature *userstore.AudioTrackSignature `json:"series_audio_preference_signature,omitempty"`
	// SelectedAudioSignature is the signature of the committed selection at
	// start time. It anchors the executable comparison at probe landing and
	// detects a user track change that superseded the start selection.
	SelectedAudioSignature *userstore.AudioTrackSignature `json:"selected_audio_signature,omitempty"`
	// AudioReconcileLedger is the settled/pending reconciliation ledger for
	// this attempt: one entry per verified generation the reconcile path has
	// decided on. Like RecoveryState it is written only by a dedicated
	// revision-checked writer (RecordAudioReconciliation), never by
	// SaveAttempt or CompleteReplan, so a stale attempt write can never
	// shrink it. Entries persist in the audio_reconcile_ledger column; the
	// memory store keeps them on the record itself. A fresh attempt starts
	// with the zero value and inherits nothing.
	AudioReconcileLedger AudioReconcileLedgerV3
	// StartResponse is the latest durable decision for this attempt. It begins
	// as the exact start response and advances atomically with each completed
	// replan so an idempotent start retry never resurrects a superseded plan.
	StartResponse DecisionResponseV3
	// RequestDigest fingerprints the normalized start request so an attempt-ID
	// reused with different input is a detectable idempotency violation rather
	// than a silent replay of the old plan.
	RequestDigest string
	ExpiresAt     time.Time
	// LastSequence and LastSample are the durable progress sequencing state
	// (see ProgressStoreV3); SaveAttempt ignores them, a new row starts at 0.
	LastSequence int64
	LastSample   *ProgressSampleV3
	// StoppedAt is set once the attempt has been stopped; a stopped attempt
	// never replays as a playable decision.
	StoppedAt *time.Time
	// LastSampleAt is when the row last accepted a progress sample or a stop.
	// A replica reaping a stale local copy compares it with its own activity
	// clock, since requests can land on other replicas.
	LastSampleAt time.Time
	// RecoveryState is the durable attempt-scoped exclusion chain: the provider
	// candidates this attempt has confirmed as failed, keyed to the release
	// scope they indict. It is written only by AppendRecoveryExclusions (a
	// revision-checked union), never by SaveAttempt or CompleteReplan, so a
	// stale attempt write can never shrink the chain. A fresh attempt starts
	// with the zero value and inherits nothing.
	RecoveryState RecoveryStateV3
	// RecoveryRevision advances with every durable RecoveryState write. It is
	// the compare-and-set token for concurrent exclusion appends: a caller
	// reads the revision with the state and a mismatched write is retried
	// against the freshly read state instead of clobbering it.
	RecoveryRevision int64
}

// RecoveryStateV3 is the durable, attempt-scoped recovery memory. The
// exclusion list records provider candidates that a confirmed verdict indicted
// so the candidate-selection loop for later replans skips them even when the
// asynchronous failed_at marker never landed.
type RecoveryStateV3 struct {
	// Exclusions is the confirmed chain, in the order verdicts arrived. It is
	// scoped by provider/source so a candidate id from one release set cannot
	// suppress an identically named candidate from another.
	Exclusions []RecoveryExclusionV3 `json:"exclusions,omitempty"`
}

// AudioSelectionV3 is the JSONB shape of the audio_selection attempt
// column: the durable start-time audio selection intent. AttemptRecordV3
// carries the same fields inline for the memory store; Postgres splits them
// into their own column so CompleteReplan (which rewrites only plan
// columns) can never clobber them.
type AudioSelectionV3 struct {
	Origin            string                         `json:"origin,omitempty"`
	PreferredLanguage string                         `json:"preferred_language,omitempty"`
	SeriesSignature   *userstore.AudioTrackSignature `json:"series_signature,omitempty"`
	SelectedSignature *userstore.AudioTrackSignature `json:"selected_signature,omitempty"`
}

// AudioReconcileLedgerV3 is the durable, attempt-scoped memory of automatic
// default-audio reconciliation decisions. One entry per verified generation
// the reconcile path has settled (a committed replan, a byte-equal no-op, or
// a refusal), keyed by the generation so a retried probe write or a second
// replica replays the same decision instead of minting a second replan.
type AudioReconcileLedgerV3 struct {
	// Entries is the settled chain, in the order decisions were recorded. It
	// is scoped by generation: entries for different generations never
	// suppress each other, and a new probe generation always re-evaluates.
	Entries []AudioReconcileEntryV3 `json:"entries,omitempty"`
	// Revision advances with every durable ledger write. It is the
	// compare-and-set token for concurrent decision records: a writer that
	// read a stale revision loses with ErrRecoveryRevisionConflictV3 instead
	// of clobbering a newer decision.
	Revision int64 `json:"revision,omitempty"`
}

// AudioReconcileDecisionV3 names the settled outcome recorded for one
// verified generation.
type AudioReconcileDecisionV3 string

const (
	// AudioReconcileReplanned means an automatic track_change replan for the
	// generation committed (or replayed idempotently) through the replan
	// lease. The entry's RequestDigest pins the exact replan body: a retry of
	// the same decision reuses the same bytes, so the lease answers with the
	// stored response instead of colliding on the request id.
	AudioReconcileReplanned AudioReconcileDecisionV3 = "replanned"
	// AudioReconcileNoop means the generation compared byte-equal against
	// the committed selection: no replan, no session mutation, no push.
	AudioReconcileNoop AudioReconcileDecisionV3 = "noop"
	// AudioReconcileRefused means the generation was stale (rotation),
	// explicit (never overridden), or superseded by a viewer change: the
	// reconcile path declined and must not retry the generation.
	AudioReconcileRefused AudioReconcileDecisionV3 = "refused"
)

// AudioReconcileEntryV3 is one settled reconciliation decision. Generation
// names the verified inventory it was decided against; Request holds the
// exact automatic replan body for a replanned decision (nil otherwise) so a
// retry replays the same bytes; RequestDigest fingerprints those bytes and
// is the idempotency key the decision is retried under.
type AudioReconcileEntryV3 struct {
	Generation    string                   `json:"generation"`
	Decision      AudioReconcileDecisionV3 `json:"decision"`
	AudioIndex    *int                     `json:"audio_index,omitempty"`
	Request       *ReplanRequestV3         `json:"request,omitempty"`
	RequestDigest string                   `json:"request_digest,omitempty"`
}

// RecoveryExclusionV3 is one confirmed candidate failure. The tuple is scoped
// so only the release the verdict actually examined is excluded:
//
//   - ProviderSource identifies the provider/source namespace (for a virtual
//     release the provider-neutral URI without the concrete result pick), so
//     two providers cannot suppress each other's identically numbered results.
//   - CandidateID is the provider result id that was confirmed bad.
//   - ReleaseID is the durable release identity (provider GUID/hash) when
//     known, so a renumbered result still matches the excluded release. Empty
//     when the release carried no durable identity.
//   - FileID is the session-bound catalog row id. It narrows the exclusion to
//     the file identity the attempt was bound to when the catalog row moved.
type RecoveryExclusionV3 struct {
	ProviderSource string `json:"provider_source,omitempty"`
	CandidateID    string `json:"candidate_id"`
	ReleaseID      string `json:"release_id,omitempty"`
	FileID         int    `json:"file_id,omitempty"`
	// ConfirmedAt is diagnostic: when the durable write recorded the verdict.
	ConfirmedAt time.Time `json:"confirmed_at,omitempty"`
}

// RecoveryExclusionKeyV3 is the dedup identity of one exclusion: two verdicts
// for the same provider source and candidate id are the same exclusion even
// when their release identity or file binding differ.
func RecoveryExclusionKeyV3(providerSource, candidateID string) string {
	return strings.TrimSpace(providerSource) + "\x00" + strings.TrimSpace(candidateID)
}

// AttemptIdentityV3 carries only the ownership columns of an attempt so
// per-event authorization checks avoid decoding the plan and request JSONB.
type AttemptIdentityV3 struct {
	PlaybackAttemptID string
	SessionID         string
	UserID            int
	ProfileID         string
}

type RouteEventRecordV3 struct {
	RouteEventV3
	// EventID is the client-minted identity of one report. Empty for legacy
	// v1 reports; a v2 report always carries one so a retry after a lost 202
	// records the event once.
	EventID       string
	UserID        int
	ProfileID     string
	ClientName    string
	ClientVersion string
	// ClientBuild and ClientChannel are opaque client-reported identifiers,
	// recorded so a route decision can be attributed to an exact app build.
	ClientBuild   string
	ClientChannel string
	ClientModel   string
}

type ReplanLeaseStateV3 string

const (
	ReplanLeaseOwnedV3     ReplanLeaseStateV3 = "owned"
	ReplanLeaseInFlightV3  ReplanLeaseStateV3 = "in_flight"
	ReplanLeaseCompletedV3 ReplanLeaseStateV3 = "completed"
)

type ReplanLeaseV3 struct {
	State ReplanLeaseStateV3
	// LeaseToken identifies one ownership generation of an active replan. It
	// is opaque to callers and must accompany release and completion writes.
	LeaseToken string
	Response   json.RawMessage
}

type PlanStoreV3 interface {
	AcquireSessionLock(context.Context, string) (func(), error)
	SaveAttempt(context.Context, AttemptRecordV3) error
	GetAttempt(context.Context, string) (*AttemptRecordV3, error)
	GetAttemptByPlaybackAttemptID(context.Context, string) (*AttemptRecordV3, error)
	GetAttemptIdentity(context.Context, string) (*AttemptIdentityV3, error)
	GetAttemptIdentityByPlaybackAttemptID(context.Context, string) (*AttemptIdentityV3, error)
	BeginReplan(context.Context, string, string, string, string, time.Time) (ReplanLeaseV3, error)
	// ReleaseReplan abandons an owned, incomplete lease after the handler fails
	// before producing a durable response. The token prevents cleanup from an
	// expired owner deleting a lease that has since been reclaimed.
	ReleaseReplan(context.Context, string, string, string) error
	// CompleteReplan commits a replan atomically; the attempt row is only
	// updated while the caller still owns the lease and its
	// current_replan_request_id equals the caller's base revision, otherwise
	// ErrReplanSupersededV3 is returned.
	CompleteReplan(ctx context.Context, sessionID, requestID, leaseToken, baseReplanRequestID string, response json.RawMessage, record AttemptRecordV3) error
	// RecordRouteEvent reports whether it stored a new row. A v2 report that
	// repeats an event_id already stored for its attempt is a no-op and
	// returns false, so derived metrics count a retried report once.
	RecordRouteEvent(context.Context, RouteEventRecordV3) (inserted bool, err error)
	CleanupExpired(context.Context, time.Time) (int64, error)
}

// RecoveryStateStoreV3 is the durable attempt-scoped exclusion chain. It is a
// separate capability so the handler can detect a store that cannot persist
// recovery state (a legacy wrapper) rather than silently dropping a confirmed
// verdict. An implementation must union, never replace: a write can only grow
// the chain, and concurrent writers serialize on the recovery revision so
// neither loses an exclusion.
type RecoveryStateStoreV3 interface {
	// AppendRecoveryExclusions adds the confirmed exclusions to the attempt's
	// durable chain and returns the resulting state. baseRevision is the
	// revision the caller read alongside its current state; a mismatched write
	// returns ErrRecoveryRevisionConflictV3 with the committed revision instead
	// of clobbering, and the caller re-reads and retries the union. A negative
	// baseRevision unions unconditionally. It returns ErrSessionNotFound when
	// there is no live attempt row.
	AppendRecoveryExclusions(ctx context.Context, sessionID string, baseRevision int64, exclusions []RecoveryExclusionV3) (RecoveryStateV3, int64, error)
	// GetRecoveryState reads the durable chain and its current revision.
	GetRecoveryState(ctx context.Context, sessionID string) (RecoveryStateV3, int64, error)
}

// AudioReconcileStoreV3 is the durable attempt-scoped reconciliation ledger.
// It is a separate capability so the handler can detect a store that cannot
// persist it (a legacy wrapper) and run the reconcile path without a ledger
// rather than silently dropping a recorded decision. An implementation must
// append, never replace: a generation is recorded once, and concurrent
// writers serialize on the ledger revision so neither loses a decision.
type AudioReconcileStoreV3 interface {
	// RecordAudioReconciliation appends the settled decision to the attempt's
	// durable ledger and returns the resulting ledger. baseRevision is the
	// revision the caller read alongside its current ledger; a mismatched
	// write returns ErrRecoveryRevisionConflictV3 with the committed revision
	// instead of clobbering, and the caller re-reads and retries. A negative
	// baseRevision appends unconditionally. Recording the same generation
	// twice is a no-op returning the stored entry. It returns
	// ErrSessionNotFound when there is no live attempt row.
	RecordAudioReconciliation(ctx context.Context, sessionID string, baseRevision int64, entry AudioReconcileEntryV3) (AudioReconcileLedgerV3, int64, error)
	// GetAudioReconcileLedger reads the durable ledger and its revision.
	GetAudioReconcileLedger(ctx context.Context, sessionID string) (AudioReconcileLedgerV3, int64, error)
}

// FindAudioReconcileEntry returns the settled entry for generation, or nil
// when the generation has no recorded decision yet.
func FindAudioReconcileEntry(ledger AudioReconcileLedgerV3, generation string) *AudioReconcileEntryV3 {
	for i := range ledger.Entries {
		if ledger.Entries[i].Generation == generation {
			entry := ledger.Entries[i]
			return &entry
		}
	}
	return nil
}

// AppendAudioReconcileEntry returns base with entry appended, or base
// unchanged when the generation is already recorded. The first writer wins;
// a replayed record is a no-op, so concurrent commits converge.
func AppendAudioReconcileEntry(base AudioReconcileLedgerV3, entry AudioReconcileEntryV3) AudioReconcileLedgerV3 {
	if FindAudioReconcileEntry(base, entry.Generation) != nil {
		return base
	}
	merged := base
	merged.Entries = append(append([]AudioReconcileEntryV3(nil), base.Entries...), entry)
	return merged
}

// UnionRecoveryExclusions appends the incoming exclusions to a copy of base,
// deduplicating on provider source + candidate id. The result preserves the
// order of base first, then the newly added entries in input order, so
// successive unions converge and a replayed append is a no-op. It is the one
// merge both the memory and Postgres stores apply, so their semantics cannot
// drift.
func UnionRecoveryExclusions(base RecoveryStateV3, incoming []RecoveryExclusionV3) RecoveryStateV3 {
	merged := make([]RecoveryExclusionV3, 0, len(base.Exclusions)+len(incoming))
	seen := make(map[string]struct{}, len(base.Exclusions)+len(incoming))
	for _, exclusion := range base.Exclusions {
		key := RecoveryExclusionKeyV3(exclusion.ProviderSource, exclusion.CandidateID)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		merged = append(merged, exclusion)
	}
	for _, exclusion := range incoming {
		if strings.TrimSpace(exclusion.CandidateID) == "" {
			continue
		}
		key := RecoveryExclusionKeyV3(exclusion.ProviderSource, exclusion.CandidateID)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		merged = append(merged, exclusion)
	}
	if len(merged) == 0 {
		return RecoveryStateV3{}
	}
	return RecoveryStateV3{Exclusions: merged}
}

// RecoveryCandidateExcludedV3 reports whether candidateID is excluded for the
// given provider source in state. A match is by candidate id under the same
// source, or by a shared non-empty release identity, so a renumbered result
// still matches the release a prior verdict indicted. An empty candidate id
// never matches.
func RecoveryCandidateExcludedV3(state RecoveryStateV3, providerSource, candidateID, releaseID string) bool {
	candidateID = strings.TrimSpace(candidateID)
	if candidateID == "" {
		return false
	}
	providerSource = strings.TrimSpace(providerSource)
	releaseID = strings.TrimSpace(releaseID)
	for _, exclusion := range state.Exclusions {
		if strings.TrimSpace(exclusion.ProviderSource) != providerSource {
			continue
		}
		if strings.TrimSpace(exclusion.CandidateID) == candidateID {
			return true
		}
		if releaseID != "" && strings.TrimSpace(exclusion.ReleaseID) == releaseID {
			return true
		}
	}
	return false
}

// RecoveryExcludedCandidateIDsV3 returns the candidate ids in state that are
// scoped to providerSource, for threading into a resolver's exclusion list.
// Order follows the stored chain so a deterministic resolver sees a stable
// list.
func RecoveryExcludedCandidateIDsV3(state RecoveryStateV3, providerSource string) []string {
	providerSource = strings.TrimSpace(providerSource)
	var ids []string
	for _, exclusion := range state.Exclusions {
		if strings.TrimSpace(exclusion.ProviderSource) != providerSource {
			continue
		}
		if id := strings.TrimSpace(exclusion.CandidateID); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

type memoryReplanV3 struct {
	digest     string
	base       string
	lease      time.Time
	leaseToken string
	completed  bool
	response   json.RawMessage
}

type MemoryPlanStoreV3 struct {
	mu           sync.Mutex
	attempts     map[string]AttemptRecordV3
	replans      map[string]memoryReplanV3
	stopReceipts map[string]StopReceiptV3
	events       []RouteEventRecordV3
}

func NewMemoryPlanStoreV3() *MemoryPlanStoreV3 {
	return &MemoryPlanStoreV3{attempts: make(map[string]AttemptRecordV3), replans: make(map[string]memoryReplanV3)}
}

// AcquireSessionLock is deliberately a no-op. The store lock exists to
// serialize replans across processes sharing one PostgreSQL database; the
// memory store only ever backs a single-process, DB-less deployment, where
// the handler's own per-session replan mutex already provides the same
// serialization before this lock is taken.
func (s *MemoryPlanStoreV3) AcquireSessionLock(context.Context, string) (func(), error) {
	return func() {}, nil
}

func (s *MemoryPlanStoreV3) SaveAttempt(_ context.Context, record AttemptRecordV3) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	// Expired rows are replaceable, mirroring the Postgres pre-delete: they
	// linger until the hourly cleanup and must not wedge a legitimate retry.
	for attemptID, existing := range s.attempts {
		if existing.ExpiresAt.After(now) {
			continue
		}
		if existing.PlaybackAttemptID == record.PlaybackAttemptID || (record.SessionID != "" && existing.SessionID == record.SessionID) {
			s.deleteAttemptLocked(attemptID)
		}
	}
	for _, existing := range s.attempts {
		if existing.PlaybackAttemptID != record.PlaybackAttemptID && (record.SessionID == "" || existing.SessionID != record.SessionID) {
			continue
		}
		if existing.PlaybackAttemptID == record.PlaybackAttemptID &&
			existing.RequestDigest != "" && record.RequestDigest != "" && existing.RequestDigest != record.RequestDigest {
			return ErrIdempotencyKeyReusedV3
		}
		return ErrPlaybackAttemptExistsV3
	}
	record.LastSequence, record.LastSample, record.StoppedAt = 0, nil, nil
	s.attempts[record.PlaybackAttemptID] = record
	return nil
}

// ReplaceAttempt overwrites a session's attempt record unconditionally. It is
// not part of PlanStoreV3: durable stores treat attempts as insert-once and
// replan-updated, so only in-memory test setups may rewrite one in place.
func (s *MemoryPlanStoreV3) ReplaceAttempt(_ context.Context, record AttemptRecordV3) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for attemptID, existing := range s.attempts {
		if existing.PlaybackAttemptID == record.PlaybackAttemptID || (record.SessionID != "" && existing.SessionID == record.SessionID) {
			// ReplaceAttempt simulates a mixed-version writer in tests. Preserve
			// its replan rows just as an UPDATE of the durable attempt would.
			delete(s.attempts, attemptID)
		}
	}
	s.attempts[record.PlaybackAttemptID] = record
}

func (s *MemoryPlanStoreV3) deleteAttemptLocked(attemptID string) {
	record, ok := s.attempts[attemptID]
	delete(s.attempts, attemptID)
	if !ok || record.SessionID == "" {
		return
	}
	delete(s.stopReceipts, record.SessionID)
	for key := range s.replans {
		if strings.HasPrefix(key, record.SessionID+":") {
			delete(s.replans, key)
		}
	}
}

func (s *MemoryPlanStoreV3) GetAttemptByPlaybackAttemptID(_ context.Context, attemptID string) (*AttemptRecordV3, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.attempts[attemptID]
	if ok && record.ExpiresAt.After(time.Now()) {
		copy := record
		return &copy, nil
	}
	return nil, ErrSessionNotFound
}

func (s *MemoryPlanStoreV3) GetAttempt(_ context.Context, sessionID string) (*AttemptRecordV3, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, record := range s.attempts {
		if record.SessionID == sessionID && record.ExpiresAt.After(time.Now()) {
			copy := record
			return &copy, nil
		}
	}
	return nil, ErrSessionNotFound
}

func (s *MemoryPlanStoreV3) BeginReplan(_ context.Context, sessionID, requestID, digest, baseReplanRequestID string, leaseUntil time.Time) (ReplanLeaseV3, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := sessionID + ":" + requestID
	existing, ok := s.replans[key]
	if !ok {
		leaseToken := uuid.NewString()
		s.replans[key] = memoryReplanV3{digest: digest, base: baseReplanRequestID, lease: leaseUntil, leaseToken: leaseToken}
		return ReplanLeaseV3{State: ReplanLeaseOwnedV3, LeaseToken: leaseToken}, nil
	}
	if existing.digest != digest {
		return ReplanLeaseV3{}, ErrIdempotencyKeyReusedV3
	}
	if existing.completed {
		return ReplanLeaseV3{State: ReplanLeaseCompletedV3, Response: append(json.RawMessage(nil), existing.response...)}, nil
	}
	if time.Now().Before(existing.lease) {
		return ReplanLeaseV3{State: ReplanLeaseInFlightV3}, nil
	}
	if existing.base != baseReplanRequestID {
		return ReplanLeaseV3{}, ErrStaleReplanLeaseV3
	}
	existing.lease = leaseUntil
	existing.leaseToken = uuid.NewString()
	s.replans[key] = existing
	return ReplanLeaseV3{State: ReplanLeaseOwnedV3, LeaseToken: existing.leaseToken}, nil
}

func (s *MemoryPlanStoreV3) ReleaseReplan(_ context.Context, sessionID, requestID, leaseToken string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := sessionID + ":" + requestID
	entry, ok := s.replans[key]
	if ok && !entry.completed && entry.leaseToken == leaseToken {
		delete(s.replans, key)
	}
	return nil
}

func (s *MemoryPlanStoreV3) CompleteReplan(_ context.Context, sessionID, requestID, leaseToken, baseReplanRequestID string, response json.RawMessage, record AttemptRecordV3) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var attemptID string
	var existing AttemptRecordV3
	for candidateID, candidate := range s.attempts {
		if candidate.SessionID == sessionID {
			attemptID, existing = candidateID, candidate
			break
		}
	}
	if attemptID == "" {
		return ErrSessionNotFound
	}
	if existing.StoppedAt != nil {
		return ErrAttemptStoppedV3
	}
	if existing.CurrentReplanRequestID != baseReplanRequestID {
		return ErrReplanSupersededV3
	}
	key := sessionID + ":" + requestID
	entry, ok := s.replans[key]
	if !ok {
		return ErrSessionNotFound
	}
	if entry.completed || entry.leaseToken != leaseToken {
		return ErrReplanSupersededV3
	}
	entry.completed = true
	entry.response = append(json.RawMessage(nil), response...)
	s.replans[key] = entry
	// Recovery state is owned by AppendRecoveryExclusions, not by the plan
	// write: the record here was read at replan start, so overwriting it would
	// drop an exclusion a concurrent verdict appended mid-replan. Postgres
	// updates only the plan columns; the memory store preserves the fields the
	// same way.
	record.RecoveryState, record.RecoveryRevision = existing.RecoveryState, existing.RecoveryRevision
	record.LastSequence, record.LastSample, record.StoppedAt = existing.LastSequence, existing.LastSample, existing.StoppedAt
	s.attempts[attemptID] = record
	return nil
}

func (s *MemoryPlanStoreV3) GetAttemptIdentity(ctx context.Context, sessionID string) (*AttemptIdentityV3, error) {
	record, err := s.GetAttempt(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return &AttemptIdentityV3{PlaybackAttemptID: record.PlaybackAttemptID, SessionID: record.SessionID, UserID: record.UserID, ProfileID: record.ProfileID}, nil
}

func (s *MemoryPlanStoreV3) GetAttemptIdentityByPlaybackAttemptID(ctx context.Context, attemptID string) (*AttemptIdentityV3, error) {
	record, err := s.GetAttemptByPlaybackAttemptID(ctx, attemptID)
	if err != nil {
		return nil, err
	}
	return &AttemptIdentityV3{PlaybackAttemptID: record.PlaybackAttemptID, SessionID: record.SessionID, UserID: record.UserID, ProfileID: record.ProfileID}, nil
}

func (s *MemoryPlanStoreV3) RecordRouteEvent(_ context.Context, record RouteEventRecordV3) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if record.EventID != "" {
		for _, existing := range s.events {
			if existing.EventID == record.EventID && existing.PlaybackAttemptID == record.PlaybackAttemptID {
				return false, nil
			}
		}
	}
	s.events = append(s.events, record)
	return true, nil
}

func (s *MemoryPlanStoreV3) CleanupExpired(_ context.Context, now time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var count int64
	for attemptID, record := range s.attempts {
		if !record.ExpiresAt.After(now) {
			s.deleteAttemptLocked(attemptID)
			count++
		}
	}
	return count, nil
}

// AppendRecoveryExclusions unions the confirmed exclusions into the attempt's
// durable chain. It is a compare-and-set on record.RecoveryRevision: a caller
// presenting a stale revision loses and its union is recomputed against the
// committed state, so no writer can drop another's exclusion. The memory store
// serializes on mu, so the compare never actually races, but the semantics
// mirror Postgres exactly.
func (s *MemoryPlanStoreV3) AppendRecoveryExclusions(_ context.Context, sessionID string, baseRevision int64, exclusions []RecoveryExclusionV3) (RecoveryStateV3, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	attemptID, record := s.findAttemptLocked(sessionID)
	if record == nil {
		return RecoveryStateV3{}, 0, ErrSessionNotFound
	}
	base := record.RecoveryState
	if baseRevision >= 0 && record.RecoveryRevision != baseRevision {
		// A concurrent writer advanced the chain after the caller's read.
		// Recompute against the committed state rather than the caller's copy.
		return RecoveryStateV3{}, record.RecoveryRevision, ErrRecoveryRevisionConflictV3
	}
	merged := UnionRecoveryExclusions(base, exclusions)
	record.RecoveryState = merged
	record.RecoveryRevision++
	s.attempts[attemptID] = *record
	return merged, record.RecoveryRevision, nil
}

// GetRecoveryState reads the durable chain and its revision.
func (s *MemoryPlanStoreV3) GetRecoveryState(_ context.Context, sessionID string) (RecoveryStateV3, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, record := s.findAttemptLocked(sessionID)
	if record == nil {
		return RecoveryStateV3{}, 0, ErrSessionNotFound
	}
	return record.RecoveryState, record.RecoveryRevision, nil
}

// RecordAudioReconciliation appends the settled decision to the attempt's
// durable ledger. It is a compare-and-set on the ledger revision: a caller
// presenting a stale revision loses and retries against the committed
// ledger, so no writer can drop another's decision. Recording the same
// generation twice returns the stored entry without growing the chain.
func (s *MemoryPlanStoreV3) RecordAudioReconciliation(_ context.Context, sessionID string, baseRevision int64, entry AudioReconcileEntryV3) (AudioReconcileLedgerV3, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	attemptID, record := s.findAttemptLocked(sessionID)
	if record == nil {
		return AudioReconcileLedgerV3{}, 0, ErrSessionNotFound
	}
	if baseRevision >= 0 && record.AudioReconcileLedger.Revision != baseRevision {
		return AudioReconcileLedgerV3{}, record.AudioReconcileLedger.Revision, ErrRecoveryRevisionConflictV3
	}
	merged := AppendAudioReconcileEntry(record.AudioReconcileLedger, entry)
	merged.Revision = record.AudioReconcileLedger.Revision + 1
	record.AudioReconcileLedger = merged
	s.attempts[attemptID] = *record
	return merged, merged.Revision, nil
}

// GetAudioReconcileLedger reads the durable ledger and its revision.
func (s *MemoryPlanStoreV3) GetAudioReconcileLedger(_ context.Context, sessionID string) (AudioReconcileLedgerV3, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, record := s.findAttemptLocked(sessionID)
	if record == nil {
		return AudioReconcileLedgerV3{}, 0, ErrSessionNotFound
	}
	return record.AudioReconcileLedger, record.AudioReconcileLedger.Revision, nil
}

// findAttemptLocked returns the live attempt for a session; the caller holds
// s.mu.
func (s *MemoryPlanStoreV3) findAttemptLocked(sessionID string) (string, *AttemptRecordV3) {
	for attemptID, record := range s.attempts {
		if record.SessionID == sessionID && record.ExpiresAt.After(time.Now()) {
			return attemptID, &record
		}
	}
	return "", nil
}

func (s *MemoryPlanStoreV3) ApplyProgress(_ context.Context, sessionID string, sample ProgressSampleV3) (ProgressReceiptV3, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	attemptID, record := s.findAttemptLocked(sessionID)
	if record == nil {
		return ProgressReceiptV3{}, ErrSessionNotFound
	}
	if record.StoppedAt != nil {
		return ProgressReceiptV3{}, ErrAttemptStoppedV3
	}
	if sample.Sequence > record.LastSequence {
		applied := sample
		record.LastSequence, record.LastSample = sample.Sequence, &applied
		record.LastSampleAt = time.Now()
		s.attempts[attemptID] = *record
		return ProgressReceiptV3{Outcome: ProgressAppliedV3, Accepted: &sample}, nil
	}
	return ResolveUnappliedProgressV3(record.LastSequence, record.LastSample, sample)
}

// ResolveUnappliedProgressV3 classifies a sample the compare-and-set rejected
// against the row's committed state; shared by the memory and Postgres stores.
func ResolveUnappliedProgressV3(lastSequence int64, last *ProgressSampleV3, sample ProgressSampleV3) (ProgressReceiptV3, error) {
	var accepted *ProgressSampleV3
	if last != nil {
		copy := *last
		accepted = &copy
	}
	if sample.Sequence == lastSequence && last != nil {
		if last.Position == sample.Position && last.IsPaused == sample.IsPaused {
			return ProgressReceiptV3{Outcome: ProgressReplayedV3, Accepted: accepted}, nil
		}
		return ProgressReceiptV3{}, ErrProgressConflictV3
	}
	return ProgressReceiptV3{Outcome: ProgressStaleSampleV3, Accepted: accepted}, nil
}

func (s *MemoryPlanStoreV3) StopAttempt(_ context.Context, sessionID, stopID string, final *ProgressSampleV3) (StopReceiptV3, bool, error) {
	if _, err := uuid.Parse(stopID); err != nil {
		return StopReceiptV3{}, false, ErrInvalidStopIDV3
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	attemptID, record := s.findAttemptLocked(sessionID)
	if record == nil {
		return StopReceiptV3{}, false, ErrSessionNotFound
	}
	if record.StoppedAt != nil {
		receipt, ok := s.stopReceipts[sessionID]
		if !ok {
			return StopReceiptV3{}, false, ErrSessionNotFound
		}
		return receipt, false, nil
	}
	if final != nil && final.Sequence > record.LastSequence {
		applied := *final
		record.LastSequence, record.LastSample = final.Sequence, &applied
	}
	now := time.Now()
	record.StoppedAt = &now
	record.LastSampleAt = now
	s.attempts[attemptID] = *record
	receipt := StopReceiptV3{StopID: stopID}
	if record.LastSample != nil {
		accepted := *record.LastSample
		receipt.Accepted = &accepted
	}
	if s.stopReceipts == nil {
		s.stopReceipts = make(map[string]StopReceiptV3)
	}
	s.stopReceipts[sessionID] = receipt
	return receipt, true, nil
}

func (s *MemoryPlanStoreV3) RecordStopReceipt(_ context.Context, sessionID string, receipt StopReceiptV3) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, record := s.findAttemptLocked(sessionID)
	if record == nil || record.StoppedAt == nil {
		return ErrSessionNotFound
	}
	if stored, ok := s.stopReceipts[sessionID]; !ok || stored.StopID != receipt.StopID {
		return ErrSessionNotFound
	}
	if receipt.Finalized {
		receipt.FinalizingUntil = time.Time{}
	}
	s.stopReceipts[sessionID] = receipt
	return nil
}

func (s *MemoryPlanStoreV3) ClaimStopFinalization(_ context.Context, sessionID string, leaseUntil time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, record := s.findAttemptLocked(sessionID)
	if record == nil || record.StoppedAt == nil {
		return false, ErrSessionNotFound
	}
	stored, ok := s.stopReceipts[sessionID]
	if !ok {
		return false, ErrSessionNotFound
	}
	if stored.Finalized || time.Now().Before(stored.FinalizingUntil) {
		return false, nil
	}
	stored.FinalizingUntil = leaseUntil
	s.stopReceipts[sessionID] = stored
	return true, nil
}

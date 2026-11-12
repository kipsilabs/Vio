package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Silo-Server/silo-server/internal/access"
	apimw "github.com/Silo-Server/silo-server/internal/api/middleware"
	"github.com/Silo-Server/silo-server/internal/auth"
	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/settingscontract"
	"github.com/Silo-Server/silo-server/internal/settingskeys"
	"github.com/Silo-Server/silo-server/internal/settingsresolve"
	"github.com/Silo-Server/silo-server/internal/userdb"
	"github.com/Silo-Server/silo-server/internal/userstore"
)

func newValuesTestHandler(t *testing.T) (*SettingValuesHandler, userstore.UserStore) {
	t.Helper()

	dsn := "file:" + strings.NewReplacer("/", "_", " ", "_").Replace(t.Name()) +
		"?mode=memory&cache=shared"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if err := userdb.InitSchema(db); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	store := userdb.NewSQLiteUserStore(db)
	if err := store.CreateProfile(context.Background(),
		userstore.Profile{ID: "profile-1", Name: "Main"}); err != nil {
		t.Fatalf("create profile: %v", err)
	}

	contract, err := settingscontract.Load()
	if err != nil {
		t.Fatalf("loading contract: %v", err)
	}
	return NewSettingValuesHandler(testUserStoreProvider{store: store}, contract), store
}

// valuesRequest builds a request carrying the session identity the handlers
// read: user, profile and device all come from context or headers rather than
// the query string, so one profile cannot address another's settings.
func valuesRequest(method, target string, body []byte) *http.Request {
	var req *http.Request
	if body == nil {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, bytes.NewReader(body))
	}
	req.Header.Set(deviceIDHeader, "device-1")
	req.Header.Set(clientFamilyHeader, "web")
	ctx := apimw.SetClaims(req.Context(), &auth.Claims{UserID: 1})
	return req.WithContext(apimw.SetProfileID(ctx, "profile-1"))
}

<<<<<<< HEAD
// routeValues wires the chi URL params the handlers read from the path.
func routeValues(t *testing.T, h *SettingValuesHandler, method, key, query string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	target := "/settings/values/" + key
	if query != "" {
		target += "?" + query
	}
	req := valuesRequest(method, target, body)

	routeCtx := chi.NewRouteContext()
	routeCtx.URLParams.Add("key", key)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, routeCtx))

	rec := httptest.NewRecorder()
	switch method {
	case http.MethodGet:
		h.HandleGetValue(rec, req)
	case http.MethodPut:
		h.HandleSetValue(rec, req)
	case http.MethodDelete:
		h.HandleDeleteValue(rec, req)
	}
	return rec
}

func routeNavigationShortcutMutation(
	t *testing.T,
	h *SettingValuesHandler,
	body string,
	mutationID string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := valuesRequest(http.MethodPut, "/settings/values/nav.shortcuts/item", []byte(body))
	if mutationID != "" {
		req.Header.Set(mutationIDHeader, mutationID)
	}
	rec := httptest.NewRecorder()
	h.HandleSetNavigationShortcut(rec, req)
	return rec
}

func decodeShortcutResponse(t *testing.T, rec *httptest.ResponseRecorder) (settingValueResponse, navigationShortcutDocument) {
	t.Helper()
	var response settingValueResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode shortcut response: %v; body=%s", err, rec.Body.String())
	}
	var document navigationShortcutDocument
	if err := json.Unmarshal(response.Value, &document); err != nil {
		t.Fatalf("decode shortcut document: %v; value=%s", err, response.Value)
	}
	return response, document
}

func TestNavigationShortcutMutationLifecycle(t *testing.T) {
	handler, _ := newValuesTestHandler(t)

	addLibrary := `{"item":{"type":"library","library_id":42,"label":"Movies"},"present":true}`
	rec := routeNavigationShortcutMutation(t, handler, addLibrary, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("add library = %d: %s", rec.Code, rec.Body.String())
	}
	response, document := decodeShortcutResponse(t, rec)
	if response.Key != settingskeys.NavShortcuts || response.Scope != "profile" || response.Revision != 1 {
		t.Fatalf("add response = %+v, want profile nav.shortcuts revision 1", response)
	}
	if len(document.Items) != 1 || document.Items[0].Label != "Movies" {
		t.Fatalf("add document = %+v, want Movies", document)
	}

	addSection := `{"item":{"type":"section","library_id":42,"section_id":"recent","label":"Recent"},"present":true}`
	rec = routeNavigationShortcutMutation(t, handler, addSection, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("add section = %d: %s", rec.Code, rec.Body.String())
	}
	response, document = decodeShortcutResponse(t, rec)
	if response.Revision != 2 || len(document.Items) != 2 || document.Items[1].SectionID != "recent" {
		t.Fatalf("add section = revision %d document %+v, want both items at revision 2", response.Revision, document)
	}

	refreshLibrary := `{"item":{"type":"library","library_id":42,"label":"Films"},"present":true}`
	rec = routeNavigationShortcutMutation(t, handler, refreshLibrary, "")
	response, document = decodeShortcutResponse(t, rec)
	if response.Revision != 3 || len(document.Items) != 2 || document.Items[0].Label != "Films" || document.Items[1].SectionID != "recent" {
		t.Fatalf("label refresh reordered or dropped items: revision %d document %+v", response.Revision, document)
	}

	// Exact desired state is a no-op: no revision churn and no duplicate.
	rec = routeNavigationShortcutMutation(t, handler, refreshLibrary, "")
	response, document = decodeShortcutResponse(t, rec)
	if response.Revision != 3 || len(document.Items) != 2 {
		t.Fatalf("repeat add = revision %d document %+v, want unchanged revision 3", response.Revision, document)
	}

	// Removal matches semantic identity only; a stale label cannot stop it.
	removeLibrary := `{"item":{"type":"library","library_id":42,"label":"Old label"},"present":false}`
	rec = routeNavigationShortcutMutation(t, handler, removeLibrary, "")
	response, document = decodeShortcutResponse(t, rec)
	if response.Revision != 4 || len(document.Items) != 1 || document.Items[0].SectionID != "recent" {
		t.Fatalf("remove library = revision %d document %+v, want section preserved", response.Revision, document)
	}

	rec = routeNavigationShortcutMutation(t, handler, removeLibrary, "")
	response, _ = decodeShortcutResponse(t, rec)
	if response.Revision != 4 {
		t.Fatalf("repeat remove revision = %d, want unchanged 4", response.Revision)
	}
}

func TestNavigationShortcutCollectionLibraryIDValidationCannotMutateExistingTargets(t *testing.T) {
	handler, store := newValuesTestHandler(t)
	global := `{"item":{"type":"collection","collection_id":"favorites","label":"All Favorites"},"present":true}`
	withinLibrary := `{"item":{"type":"collection","library_id":42,"collection_id":"favorites","label":"Movie Favorites"},"present":true}`
	for _, tc := range []struct {
		name string
		body string
	}{
		{"omitted library id is global", global},
		{"positive library id", withinLibrary},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := routeNavigationShortcutMutation(t, handler, tc.body, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("valid collection = %d: %s", rec.Code, rec.Body.String())
			}
		})
	}

	invalidLibraryIDs := map[string]string{
		"explicit null": `null`,
		"string":        `"42"`,
		"zero":          `0`,
	}
	for name, libraryID := range invalidLibraryIDs {
		t.Run(name, func(t *testing.T) {
			body := fmt.Sprintf(
				`{"item":{"type":"collection","library_id":%s,"collection_id":"favorites","label":"Ignored"},"present":false}`,
				libraryID,
			)
			rec := routeNavigationShortcutMutation(t, handler, body, "")
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_value") {
				t.Fatalf("invalid collection library_id = %d: %s", rec.Code, rec.Body.String())
			}

			stored, err := store.GetSettingValue(context.Background(), userstore.SettingIdentity{
				Key: settingskeys.NavShortcuts, Scope: settingscontract.ScopeProfile, ProfileID: "profile-1",
			})
			if err != nil || stored == nil {
				t.Fatalf("read catalog after rejected remove: value=%+v err=%v", stored, err)
			}
			var document navigationShortcutDocument
			if err := json.Unmarshal(stored.Value, &document); err != nil {
				t.Fatalf("decode catalog after rejected remove: %v", err)
			}
			if stored.Revision != 2 || len(document.Items) != 2 {
				t.Fatalf("rejected remove mutated catalog: revision %d items %+v", stored.Revision, document.Items)
			}
			if document.Items[0].LibraryID != nil {
				t.Fatalf("global collection gained library identity: %+v", document.Items[0])
			}
			if document.Items[1].LibraryID == nil || *document.Items[1].LibraryID != 42 {
				t.Fatalf("library-specific collection changed identity: %+v", document.Items[1])
			}
		})
	}

	removeGlobal := `{"item":{"type":"collection","collection_id":"favorites","label":"Ignored"},"present":false}`
	rec := routeNavigationShortcutMutation(t, handler, removeGlobal, "")
	response, document := decodeShortcutResponse(t, rec)
	if response.Revision != 3 || len(document.Items) != 1 || document.Items[0].LibraryID == nil || *document.Items[0].LibraryID != 42 {
		t.Fatalf("global removal = revision %d document %+v, want scoped collection preserved", response.Revision, document)
	}
}

func TestNavigationShortcutMutationIdempotency(t *testing.T) {
	handler, store := newValuesTestHandler(t)
	handler.storeProvider = notifications.WrapUserStoreProvider(testUserStoreProvider{store: store}, &notifications.System{})
	const mutationID = "d615e91d-988f-4928-bb32-8956a26c7608"
	body := `{"item":{"type":"collection","collection_id":"watchlist","label":"Watchlist"},"present":true}`
	first := routeNavigationShortcutMutation(t, handler, body, mutationID)
	if first.Code != http.StatusOK {
		t.Fatalf("first mutation = %d: %s", first.Code, first.Body.String())
	}
	replay := routeNavigationShortcutMutation(t, handler, body, mutationID)
	if replay.Code != http.StatusOK || replay.Header().Get("X-Vio-Idempotent-Replay") != "true" {
		t.Fatalf("replay = %d header %q: %s", replay.Code, replay.Header().Get("X-Vio-Idempotent-Replay"), replay.Body.String())
	}
	if strings.TrimSpace(replay.Body.String()) != strings.TrimSpace(first.Body.String()) {
		t.Fatalf("replay body = %s, want exact %s", replay.Body.String(), first.Body.String())
	}

	conflict := routeNavigationShortcutMutation(t, handler,
		`{"item":{"type":"collection","collection_id":"watchlist","label":"Renamed"},"present":true}`,
		mutationID)
	if conflict.Code != http.StatusConflict || !strings.Contains(conflict.Body.String(), "mutation_id_conflict") {
		t.Fatalf("mutation id reuse = %d: %s", conflict.Code, conflict.Body.String())
	}

	// A remove's label is not part of its semantics or request fingerprint.
	const removeID = "f2f65f5e-918a-46bf-bee0-65a84491d298"
	removed := routeNavigationShortcutMutation(t, handler,
		`{"item":{"type":"collection","collection_id":"watchlist","label":"First"},"present":false}`,
		removeID)
	removeReplay := routeNavigationShortcutMutation(t, handler,
		`{"item":{"type":"collection","collection_id":"watchlist","label":"Second"},"present":false}`,
		removeID)
	if removed.Code != http.StatusOK || removeReplay.Code != http.StatusOK || removeReplay.Header().Get("X-Vio-Idempotent-Replay") != "true" {
		t.Fatalf("semantic remove replay = %d/%d header %q", removed.Code, removeReplay.Code, removeReplay.Header().Get("X-Vio-Idempotent-Replay"))
	}
}

func TestNavigationShortcutMutationThroughNotificationWrappedProvider(t *testing.T) {
	baseHandler, store := newValuesTestHandler(t)
	provider := notifications.WrapUserStoreProvider(
		testUserStoreProvider{store: store},
		&notifications.System{},
	)
	handler := NewSettingValuesHandler(provider, baseHandler.contract)
	const mutationID = "wrapped-provider-mutation"
	body := `{"item":{"type":"collection","collection_id":"watchlist","label":"Watchlist"},"present":true}`

	first := routeNavigationShortcutMutation(t, handler, body, mutationID)
	if first.Code != http.StatusOK {
		t.Fatalf("wrapped first mutation = %d: %s", first.Code, first.Body.String())
	}
	replay := routeNavigationShortcutMutation(t, handler, body, mutationID)
	if replay.Code != http.StatusOK || replay.Header().Get("X-Vio-Idempotent-Replay") != "true" {
		t.Fatalf("wrapped replay = %d header %q: %s",
			replay.Code, replay.Header().Get("X-Vio-Idempotent-Replay"), replay.Body.String())
	}
	if !bytes.Equal(bytes.TrimSpace(first.Body.Bytes()), bytes.TrimSpace(replay.Body.Bytes())) {
		t.Fatalf("wrapped replay body = %s, want %s", replay.Body.String(), first.Body.String())
	}
}

func TestNavigationShortcutConcurrentSameMutationID(t *testing.T) {
	for _, tc := range []struct {
		name          string
		bodies        []string
		wantConflicts int
	}{
		{
			name: "same hash replays one receipt",
			bodies: []string{
				`{"item":{"type":"library","library_id":42,"label":"Movies"},"present":true}`,
				`{"item":{"type":"library","library_id":42,"label":"Movies"},"present":true}`,
			},
		},
		{
			name: "different hash conflicts before CAS",
			bodies: []string{
				`{"item":{"type":"library","library_id":42,"label":"Movies"},"present":true}`,
				`{"item":{"type":"library","library_id":99,"label":"Shows"},"present":true}`,
			},
			wantConflicts: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler, store := newValuesTestHandler(t)
			const mutationID = "concurrent-same-id"
			start := make(chan struct{})
			responses := make(chan *httptest.ResponseRecorder, len(tc.bodies))
			var ready sync.WaitGroup
			ready.Add(len(tc.bodies))
			for _, body := range tc.bodies {
				body := body
				go func() {
					ready.Done()
					<-start
					responses <- routeNavigationShortcutMutation(t, handler, body, mutationID)
				}()
			}
			ready.Wait()
			close(start)

			conflicts := 0
			replays := 0
			var successes [][]byte
			for range tc.bodies {
				rec := <-responses
				switch rec.Code {
				case http.StatusOK:
					successes = append(successes, bytes.TrimSpace(rec.Body.Bytes()))
					if rec.Header().Get("X-Vio-Idempotent-Replay") == "true" {
						replays++
					}
				case http.StatusConflict:
					if !strings.Contains(rec.Body.String(), "mutation_id_conflict") {
						t.Fatalf("conflict = %s, want mutation_id_conflict", rec.Body.String())
					}
					conflicts++
				default:
					t.Fatalf("concurrent mutation = %d: %s", rec.Code, rec.Body.String())
				}
			}
			if conflicts != tc.wantConflicts {
				t.Fatalf("conflicts = %d, want %d", conflicts, tc.wantConflicts)
			}
			if tc.wantConflicts == 0 {
				if len(successes) != 2 || replays != 1 || !bytes.Equal(successes[0], successes[1]) {
					t.Fatalf("same-hash outcomes: successes=%d replays=%d bodies=%q", len(successes), replays, successes)
				}
			} else if len(successes) != 1 || replays != 0 {
				t.Fatalf("different-hash outcomes: successes=%d replays=%d", len(successes), replays)
			}

			stored, err := store.GetSettingValue(context.Background(), userstore.SettingIdentity{
				Key: settingskeys.NavShortcuts, Scope: settingscontract.ScopeProfile, ProfileID: "profile-1",
			})
			if err != nil || stored == nil || stored.Revision != 1 {
				t.Fatalf("stored shortcut = %+v (%v), want exactly one CAS revision", stored, err)
			}
			receipt, err := store.GetSettingMutation(context.Background(), mutationID)
			if err != nil || receipt == nil {
				t.Fatalf("stored receipt = %+v (%v)", receipt, err)
			}
			if len(successes) > 0 && !bytes.Equal(successes[0], bytes.TrimSpace(receipt.Result)) {
				t.Fatalf("success = %s, receipt = %s", successes[0], receipt.Result)
			}
		})
	}
}

func TestNavigationShortcutMutationValidationAndWholeDocumentGuard(t *testing.T) {
	handler, store := newValuesTestHandler(t)

	wholeDocument := routeValues(t, handler, http.MethodPut, settingskeys.NavShortcuts,
		"scope=profile", []byte(`{"value":{"items":[]}}`))
	if wholeDocument.Code != http.StatusBadRequest || !strings.Contains(wholeDocument.Body.String(), "atomic_update_required") {
		t.Fatalf("whole-document PUT = %d: %s", wholeDocument.Code, wholeDocument.Body.String())
	}
	if value, err := store.GetSettingValue(context.Background(), userstore.SettingIdentity{
		Key: settingskeys.NavShortcuts, Scope: settingscontract.ScopeProfile, ProfileID: "profile-1",
	}); err != nil || value != nil {
		t.Fatalf("guarded PUT stored %+v, err=%v", value, err)
	}

	invalidBodies := []string{
		`{"item":{"type":"builtin","destination":"home","label":"Home"},"present":true}`,
		`{"item":{"type":"library","library_id":42,"label":"Movies","extra":true},"present":true}`,
		`{"item":{"type":"library","library_id":42,"label":"Movies"},"present":true,"extra":true}`,
		`{"item":{"type":"section","library_id":42,"section_id":"","label":"Recent"},"present":true}`,
		`{"item":{"type":"collection","collection_id":"favorites","label":"   "},"present":true}`,
		`{"item":{"type":"library","library_id":0,"label":"Movies"},"present":true}`,
		`{"item":{"type":"library","library_id":42,"label":"Movies"}}`,
	}
	for _, body := range invalidBodies {
		rec := routeNavigationShortcutMutation(t, handler, body, "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("invalid body %s = %d: %s", body, rec.Code, rec.Body.String())
		}
	}
}

func TestNavigationShortcutWholeDocumentDeleteCannotCreateRevisionABA(t *testing.T) {
	handler, store := newValuesTestHandler(t)
	first := routeNavigationShortcutMutation(t, handler,
		`{"item":{"type":"library","library_id":42,"label":"Movies"},"present":true}`, "")
	firstResponse, _ := decodeShortcutResponse(t, first)
	if first.Code != http.StatusOK || firstResponse.Revision != 1 {
		t.Fatalf("seed shortcut = %d revision %d: %s", first.Code, firstResponse.Revision, first.Body.String())
	}

	identity := userstore.SettingIdentity{
		Key: settingskeys.NavShortcuts, Scope: settingscontract.ScopeProfile, ProfileID: "profile-1",
	}
	beforeDelete, err := store.GetSettingValue(context.Background(), identity)
	if err != nil || beforeDelete == nil {
		t.Fatalf("read shortcut before guarded delete: value=%+v err=%v", beforeDelete, err)
	}
	deleteRec := routeValues(t, handler, http.MethodDelete, settingskeys.NavShortcuts,
		"scope=profile", nil)
	if deleteRec.Code != http.StatusBadRequest || !strings.Contains(deleteRec.Body.String(), "atomic_update_required") {
		t.Fatalf("whole-document DELETE = %d: %s", deleteRec.Code, deleteRec.Body.String())
	}

	second := routeNavigationShortcutMutation(t, handler,
		`{"item":{"type":"library","library_id":99,"label":"Shows"},"present":true}`, "")
	secondResponse, secondDocument := decodeShortcutResponse(t, second)
	if second.Code != http.StatusOK || secondResponse.Revision != 2 || len(secondDocument.Items) != 2 {
		t.Fatalf("second shortcut = %d revision %d document %+v", second.Code, secondResponse.Revision, secondDocument)
	}

	// If session DELETE had removed and recreated the row, its revision would
	// return to one and this stale writer could pass an ABA-shaped precondition.
	// Keeping the row alive makes the stale revision conflict instead.
	cas := store.(userstore.SettingValueCompareAndSetter)
	if _, err := cas.CompareAndSetSettingValue(context.Background(), identity,
		beforeDelete.Value, beforeDelete.Revision); !errors.Is(err, userstore.ErrSettingValueRevisionConflict) {
		t.Fatalf("stale CAS after guarded DELETE error = %v, want revision conflict", err)
	}
	final, err := store.GetSettingValue(context.Background(), identity)
	if err != nil || final == nil || final.Revision != 2 {
		t.Fatalf("final shortcut row = %+v err=%v, want revision 2", final, err)
	}
}

func TestNavigationShortcutMutationEnforcesDocumentCap(t *testing.T) {
	handler, store := newValuesTestHandler(t)
	items := make([]navigationShortcutItem, 0, 256)
	for id := 1; id <= 256; id++ {
		libraryID := id
		items = append(items, navigationShortcutItem{Type: "library", LibraryID: &libraryID, Label: fmt.Sprintf("Library %d", id)})
	}
	raw, err := json.Marshal(navigationShortcutDocument{Items: items})
	if err != nil {
		t.Fatal(err)
	}
	def, _ := handler.contract.Lookup(settingskeys.NavShortcuts)
	normalized, err := def.ValueSchema.NormalizeValue(raw, settingscontract.ObjectSchemas())
	if err != nil {
		t.Fatalf("normalize seed: %v", err)
	}
	identity := userstore.SettingIdentity{
		Key: settingskeys.NavShortcuts, Scope: settingscontract.ScopeProfile, ProfileID: "profile-1",
	}
	seed, err := store.UpsertSettingValue(context.Background(), identity, normalized)
	if err != nil {
		t.Fatalf("seed full catalog: %v", err)
	}

	rec := routeNavigationShortcutMutation(t, handler,
		`{"item":{"type":"library","library_id":257,"label":"Too Many"},"present":true}`, "")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_value") {
		t.Fatalf("257th shortcut = %d: %s", rec.Code, rec.Body.String())
	}
	got, err := store.GetSettingValue(context.Background(), identity)
	if err != nil || got == nil || got.Revision != seed.Revision {
		t.Fatalf("catalog after rejected add = %+v, err=%v; want unchanged revision %d", got, err, seed.Revision)
	}
}

=======
>>>>>>> upstream/main
type synchronizedShortcutStore struct {
	userstore.UserStore
	cas   userstore.SettingValueCompareAndSetter
	reads atomic.Int32
	ready chan struct{}
	once  sync.Once
	casMu sync.Mutex
}

func (s *synchronizedShortcutStore) GetSettingValue(ctx context.Context, id userstore.SettingIdentity) (*userstore.SettingValue, error) {
	value, err := s.UserStore.GetSettingValue(ctx, id)
	if err == nil && id.Key == settingskeys.NavShortcuts {
		read := s.reads.Add(1)
		if read <= 2 {
			if read == 2 {
				s.once.Do(func() { close(s.ready) })
			}
			<-s.ready
		}
	}
	return value, err
}

func (s *synchronizedShortcutStore) CompareAndSetSettingValue(
	ctx context.Context,
	id userstore.SettingIdentity,
	value json.RawMessage,
	expectedRevision int64,
) (*userstore.SettingValue, error) {
	s.casMu.Lock()
	defer s.casMu.Unlock()
	return s.cas.CompareAndSetSettingValue(ctx, id, value, expectedRevision)
}

func TestMutationHashPreservesExistingScopesAndSeparatesClientFamilies(t *testing.T) {
	identity := userstore.SettingIdentity{
		Key: "playback.subtitle_mode", Scope: settingscontract.ScopeProfileDevice,
		ProfileID: "profile-1", DeviceID: "device-1",
	}
	value := json.RawMessage(`"always"`)
	legacyPayload := append([]byte(
		"playback.subtitle_mode\x00profile_device\x00profile-1\x00device-1\x000\x00\x00",
	), value...)
	want := sha256.Sum256(legacyPayload)
	if got := hashMutationRequest(identity, value); got != hex.EncodeToString(want[:]) {
		t.Fatalf("existing-scope mutation hash changed across the profile_client rollout: %s", got)
	}

	client := userstore.SettingIdentity{
		Key: settingskeys.NavPrimaryMenu, Scope: settingscontract.ScopeProfileClient,
		ProfileID: "profile-1", ClientFamily: settingscontract.ClientFamilyTV,
	}
	tv := hashMutationRequest(client, json.RawMessage(`null`))
	client.ClientFamily = settingscontract.ClientFamilyMobile
	if mobile := hashMutationRequest(client, json.RawMessage(`null`)); mobile == tv {
		t.Fatal("profile_client mutation hashes do not distinguish client families")
	}
}

type settingValuesLibraryLookup struct {
	existing map[int]bool
	err      error
}

func (l settingValuesLibraryLookup) GetByID(_ context.Context, id int) (*models.MediaFolder, error) {
	if l.err != nil {
		return nil, l.err
	}
	if !l.existing[id] {
		return nil, catalog.ErrFolderNotFound
	}
	return &models.MediaFolder{ID: id}, nil
}

<<<<<<< HEAD
// Resetting a series' subtitle preference is idempotent: the player and the
// item page's "Auto" clear a per-series language/mode override for every title
// the viewer touches, and a series that was never overridden already resolves
// the profile default. Answering 404 there turned a working reset into a
// failed request on every such title. Other keys keep the 404.
func TestDeleteValueSubtitlePreferenceIsIdempotent(t *testing.T) {
	handler, _ := newValuesTestHandler(t)

	for _, test := range []struct{ key, value string }{
		{settingskeys.PlaybackSubtitleLanguage, `{"value":"ja"}`},
		{settingskeys.PlaybackSubtitleMode, `{"value":"always"}`},
	} {
		t.Run(test.key, func(t *testing.T) {
			query := "scope=profile_series&series_id=series-1"
			if rec := routeValues(t, handler, http.MethodDelete, test.key, query, nil); rec.Code != http.StatusNoContent {
				t.Fatalf("DELETE with nothing stored = %d, want 204: %s", rec.Code, rec.Body.String())
			}
			// A stored override still clears normally, and a repeated clear is
			// still a no-op success.
			if rec := routeValues(t, handler, http.MethodPut, test.key, query, []byte(test.value)); rec.Code != http.StatusOK {
				t.Fatalf("PUT = %d: %s", rec.Code, rec.Body.String())
			}
			if rec := routeValues(t, handler, http.MethodDelete, test.key, query, nil); rec.Code != http.StatusNoContent {
				t.Fatalf("DELETE after write = %d, want 204: %s", rec.Code, rec.Body.String())
			}
			if rec := routeValues(t, handler, http.MethodDelete, test.key, query, nil); rec.Code != http.StatusNoContent {
				t.Fatalf("repeated DELETE = %d, want 204: %s", rec.Code, rec.Body.String())
			}
		})
	}

	// The idempotent answer is scoped to the subtitle preferences: every other
	// key still reports that nothing was set at the addressed scope.
	if rec := routeValues(t, handler, http.MethodDelete, settingskeys.PlaybackIntroSkipMode,
		"scope=profile", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("DELETE of an unset non-subtitle key = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

func TestSettingValuesRejectNonexistentLibraryContext(t *testing.T) {
	handler, store := newValuesTestHandler(t)
	handler.SetLibraryLookup(settingValuesLibraryLookup{existing: map[int]bool{7: true}})

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		var body []byte
		if method == http.MethodPut {
			body = []byte(`{"value":"de"}`)
		}
		rec := routeValues(t, handler, method, "playback.subtitle_language",
			"scope=profile_library&library_id=99", body)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s nonexistent library = %d, want 404: %s", method, rec.Code, rec.Body.String())
		}
	}
	value, err := store.GetSettingValue(context.Background(), userstore.SettingIdentity{
		Key: "playback.subtitle_language", Scope: settingscontract.ScopeProfileLibrary,
		ProfileID: "profile-1", LibraryID: 99,
	})
	if err != nil || value != nil {
		t.Fatalf("nonexistent library left value (%+v, %v)", value, err)
	}

	if rec := routeValues(t, handler, http.MethodPut, "playback.subtitle_language",
		"scope=profile_library&library_id=7", []byte(`{"value":"de"}`)); rec.Code != http.StatusOK {
		t.Errorf("existing library write = %d: %s", rec.Code, rec.Body.String())
	}
}

// TestUnknownKeysAreRefused is the extension bag closing. The legacy endpoint
// stored any unregistered key as an unvalidated string, which is how six ui.*
// settings and five orphan keys reached production untyped.
func TestUnknownKeysAreRefused(t *testing.T) {
	handler, _ := newValuesTestHandler(t)

	rec := routeValues(t, handler, http.MethodPut, "totally.invented.key",
		"scope=profile", []byte(`{"value":"x"}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("PUT of an unknown key = %d, want 404: %s", rec.Code, rec.Body.String())
	}

	// A contract-known local setting is not server storage either.
	rec = routeValues(t, handler, http.MethodPut, "downloads.wifi_only",
		"scope=profile", []byte(`{"value":true}`))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("PUT of a client_local key = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

// TestScopeMustBeAllowedByTheContract. A definition declares where it may be
// written; the identity being well-formed is a separate question.
func TestScopeMustBeAllowedByTheContract(t *testing.T) {
	handler, _ := newValuesTestHandler(t)

	// ui.custom_css is profile-only, so a device override is refused.
	rec := routeValues(t, handler, http.MethodPut, "ui.custom_css",
		"scope=profile_device", []byte(`{"value":"body{}"}`))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("device write to a profile-only setting = %d, want 400: %s",
			rec.Code, rec.Body.String())
	}

	// A missing scope is a request error rather than a silent default: writing
	// to the wrong scope is exactly the mistake this API exists to prevent.
	rec = routeValues(t, handler, http.MethodPut, "playback.subtitle_mode", "",
		[]byte(`{"value":"always"}`))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("write with no scope = %d, want 400", rec.Code)
	}
}

// TestLibraryAndSeriesScopesNeedTheirIdentity.
func TestLibraryAndSeriesScopesNeedTheirIdentity(t *testing.T) {
	handler, _ := newValuesTestHandler(t)

	if rec := routeValues(t, handler, http.MethodPut, "playback.subtitle_language",
		"scope=profile_library", []byte(`{"value":"de"}`)); rec.Code != http.StatusBadRequest {
		t.Errorf("library scope without library_id = %d, want 400", rec.Code)
	}
	if rec := routeValues(t, handler, http.MethodPut, "playback.subtitle_language",
		"scope=profile_series", []byte(`{"value":"de"}`)); rec.Code != http.StatusBadRequest {
		t.Errorf("series scope without series_id = %d, want 400", rec.Code)
	}

	if rec := routeValues(t, handler, http.MethodPut, "playback.subtitle_language",
		"scope=profile_library&library_id=7", []byte(`{"value":"de"}`)); rec.Code != http.StatusOK {
		t.Errorf("library write = %d, want 200", rec.Code)
	}
	if rec := routeValues(t, handler, http.MethodPut, "playback.subtitle_language",
		"scope=profile_series&series_id=s1", []byte(`{"value":"ja"}`)); rec.Code != http.StatusOK {
		t.Errorf("series write = %d, want 200", rec.Code)
	}
}

// TestEffectiveResolvesThroughTheLadder proves the route is wired to the real
// resolver rather than reading one scope.
func TestEffectiveResolvesThroughTheLadder(t *testing.T) {
	handler, _ := newValuesTestHandler(t)

	write := func(query, value string) {
		t.Helper()
		if rec := routeValues(t, handler, http.MethodPut, "playback.subtitle_language",
			query, []byte(`{"value":`+value+`}`)); rec.Code != http.StatusOK {
			t.Fatalf("seeding %s = %d: %s", query, rec.Code, rec.Body.String())
		}
	}
	write("scope=profile", `"en"`)
	write("scope=profile_device", `"de"`)
	write("scope=profile_series&series_id=s1", `"ja"`)

	effective := func(query string) effectiveSettingValueResponse {
		t.Helper()
		req := valuesRequest(http.MethodGet, "/settings/values/effective?"+query, nil)
		rec := httptest.NewRecorder()
		handler.HandleGetEffective(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("effective %s = %d: %s", query, rec.Code, rec.Body.String())
		}
		var body struct {
			Settings []effectiveSettingValueResponse `json:"settings"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decoding: %v", err)
		}
		if len(body.Settings) != 1 {
			t.Fatalf("got %d settings, want 1", len(body.Settings))
		}
		return body.Settings[0]
	}

	// Without a series context the device override is the most specific match.
	got := effective("keys=playback.subtitle_language")
	if string(got.Value) != `"de"` || got.Source != "profile_device" {
		t.Errorf("no-series resolution = %s from %s, want \"de\" from profile_device",
			got.Value, got.Source)
	}

	// Naming the series promotes its override.
	got = effective("keys=playback.subtitle_language&series_ids=s1")
	if string(got.Value) != `"ja"` || got.Source != "profile_series" {
		t.Errorf("series resolution = %s from %s, want \"ja\" from profile_series",
			got.Value, got.Source)
	}
	if got.SeriesID != "s1" {
		t.Errorf("resolved identity series = %q, want s1 so a client can reset it", got.SeriesID)
	}
}

func TestPostEffectiveRejectsInvalidContexts(t *testing.T) {
	handler, _ := newValuesTestHandler(t)
	for name, body := range map[string]string{
		"empty":           `{"keys":["ui.custom_css"],"contexts":[]}`,
		"duplicate id":    `{"keys":["ui.custom_css"],"contexts":[{"context_id":"x","series_id":"s1"},{"context_id":"x","series_id":"s2"}]}`,
		"missing content": `{"keys":["ui.custom_css"],"contexts":[{"context_id":"x"}]}`,
		"invalid library": `{"keys":["ui.custom_css"],"contexts":[{"context_id":"x","library_id":"nope"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			req := valuesRequest(http.MethodPost, "/settings/values/effective", []byte(body))
			rec := httptest.NewRecorder()
			handler.HandlePostEffective(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestEffectiveRejectsUnknownKeys. Omitting an unknown key silently lets a
// client fill the gap with its own vendored default and present a value this
// server would refuse to store.
func TestEffectiveRejectsUnknownKeys(t *testing.T) {
	handler, _ := newValuesTestHandler(t)

	req := valuesRequest(http.MethodGet,
		"/settings/values/effective?keys=playback.subtitle_mode,totally.invented.key", nil)
	rec := httptest.NewRecorder()
	handler.HandleGetEffective(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown key = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "totally.invented.key") {
		t.Errorf("the error does not name the offending key: %s", rec.Body.String())
	}
}

// TestEffectiveResolvesDeviceAwareKeysWithoutDeviceIdentity: an effective read
// that names no device resolves the layers it can. A device-capable key is not
// an error without the device header; the resolver skips profile_device
// candidates, the response's source names the winning profile/account/default
// row, and a client that wants device resolution still sends the header. This
// replaced a fail-closed 400 that left playback clients which cannot send a
// device header with no effective values at all.
func TestEffectiveResolvesDeviceAwareKeysWithoutDeviceIdentity(t *testing.T) {
	handler, _ := newValuesTestHandler(t)

	effective := func(query string) *httptest.ResponseRecorder {
		req := valuesRequest(http.MethodGet, "/settings/values/effective?"+query, nil)
		req.Header.Del(deviceIDHeader)
		rec := httptest.NewRecorder()
		handler.HandleGetEffective(rec, req)
		return rec
	}

	// playback.subtitle_language allows profile_device: without a device
	// identity it resolves the non-device layers instead of refusing.
	if rec := effective("keys=playback.subtitle_language"); rec.Code != http.StatusOK {
		t.Errorf("device-aware key without a device id = %d, want 200: %s",
			rec.Code, rec.Body.String())
	}
	// The no-keys form resolves every remote definition, device-aware included.
	if rec := effective(""); rec.Code != http.StatusOK {
		t.Errorf("all-keys request without a device id = %d, want 200: %s",
			rec.Code, rec.Body.String())
	}
	// ui.custom_css is profile-only: it never needed a device identity.
	if rec := effective("keys=ui.custom_css"); rec.Code != http.StatusOK {
		t.Errorf("profile-only key without a device id = %d, want 200: %s",
			rec.Code, rec.Body.String())
	}
}

// TestEffectiveAppliesViewerQualityCap wires the preferences-versus-restrictions
// seam end to end: the access scope's MaxPlaybackQuality becomes the resolver's
// ceiling, the effective value is the cap, and the authored preference survives
// untouched so it takes effect the day the cap lifts.
func TestEffectiveAppliesViewerQualityCap(t *testing.T) {
	handler, store := newValuesTestHandler(t)

	if rec := routeValues(t, handler, http.MethodPut, "playback.preferred_quality",
		"scope=profile", []byte(`{"value":"2160p"}`)); rec.Code != http.StatusOK {
		t.Fatalf("seeding preference = %d: %s", rec.Code, rec.Body.String())
	}

	effective := func(maxQuality string) effectiveSettingValueResponse {
		t.Helper()
		req := valuesRequest(http.MethodGet,
			"/settings/values/effective?keys=playback.preferred_quality", nil)
		req = req.WithContext(access.SetScope(req.Context(), access.Scope{
			UserID:             1,
			ProfileID:          "profile-1",
			MaxPlaybackQuality: maxQuality,
		}))
		rec := httptest.NewRecorder()
		handler.HandleGetEffective(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("effective = %d: %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Settings []effectiveSettingValueResponse `json:"settings"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decoding: %v", err)
		}
		if len(body.Settings) != 1 {
			t.Fatalf("got %d settings, want 1", len(body.Settings))
		}
		return body.Settings[0]
	}

	// Capped at 1080p: the cap is the answer, the choice is reported alongside.
	got := effective("1080p")
	if string(got.Value) != `"1080p"` {
		t.Errorf("capped effective = %s, want \"1080p\"", got.Value)
	}
	if !got.Constrained || got.ConstraintKind != string(settingscontract.ConstraintCeiling) {
		t.Errorf("constrained=%v kind=%q, want true/ceiling", got.Constrained, got.ConstraintKind)
	}
	if string(got.StoredValue) != `"2160p"` {
		t.Errorf("stored_value = %s, want the authored \"2160p\" reported", got.StoredValue)
	}
	if string(got.RequestedValue) != `"2160p"` {
		t.Errorf("requested_value = %s, want authored 2160p", got.RequestedValue)
	}
	if got.ConstrainedBy == nil || got.ConstrainedBy.PolicyInput != policyInputMaxPlaybackQuality ||
		got.ConstrainedBy.Constraint != settingscontract.ConstraintCeiling {
		t.Errorf("constrained_by = %#v", got.ConstrainedBy)
	}
	if len(got.PermittedValues) == 0 || string(got.PermittedValues[len(got.PermittedValues)-1]) != `"1080p"` {
		t.Errorf("permitted_values = %q, want choices through 1080p", got.PermittedValues)
	}

	// The stored row itself was not rewritten by resolution.
	stored, err := store.GetSettingValue(context.Background(), userstore.SettingIdentity{
		Key:       "playback.preferred_quality",
		Scope:     settingscontract.ScopeProfile,
		ProfileID: "profile-1",
	})
	if err != nil || stored == nil {
		t.Fatalf("reading stored value: %v", err)
	}
	if string(stored.Value) != `"2160p"` {
		t.Errorf("stored row = %s, want \"2160p\" untouched", stored.Value)
	}

	// An uncapped viewer ("" means the policy sets no cap) gets the preference
	// as authored, with no constraint reported.
	got = effective("")
	if string(got.Value) != `"2160p"` || got.Constrained {
		t.Errorf("uncapped effective = %s constrained=%v, want \"2160p\"/false",
			got.Value, got.Constrained)
	}
	if got.StoredValue != nil {
		t.Errorf("stored_value = %s, want absent when nothing was narrowed", got.StoredValue)
	}
}

// TestMutationIDMakesWritesIdempotent covers the retry a mobile client performs
// when a response is lost.
func TestMutationIDMakesWritesIdempotent(t *testing.T) {
	handler, store := newValuesTestHandler(t)

	send := func(mutationID, value string) *httptest.ResponseRecorder {
		req := valuesRequest(http.MethodPut,
			"/settings/values/playback.subtitle_mode?scope=profile",
			[]byte(`{"value":`+value+`}`))
		req.Header.Set(mutationIDHeader, mutationID)
		routeCtx := chi.NewRouteContext()
		routeCtx.URLParams.Add("key", "playback.subtitle_mode")
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, routeCtx))
		rec := httptest.NewRecorder()
		handler.HandleSetValue(rec, req)
		return rec
	}

	first := send("mut-1", `"always"`)
	if first.Code != http.StatusOK {
		t.Fatalf("first write = %d: %s", first.Code, first.Body.String())
	}

	// The same id and body replays the receipt rather than writing again.
	replay := send("mut-1", `"always"`)
	if replay.Code != http.StatusOK {
		t.Fatalf("replay = %d: %s", replay.Code, replay.Body.String())
	}
	if replay.Header().Get("X-Vio-Idempotent-Replay") != "true" {
		t.Error("a repeated mutation id was not reported as a replay")
	}

	if strings.TrimSpace(first.Body.String()) != strings.TrimSpace(replay.Body.String()) {
		t.Errorf("replay body diverged from the original response:\n first: %s\nreplay: %s",
			first.Body.String(), replay.Body.String())
	}
	var original settingValueResponse
	if err := json.Unmarshal(first.Body.Bytes(), &original); err != nil {
		t.Fatalf("decoding original response: %v", err)
	}
	if original.Revision == 0 || original.UpdatedAt == "" {
		t.Errorf("original response revision=%d updated_at=%q — the replayed "+
			"receipt must carry the stored row, not the request",
			original.Revision, original.UpdatedAt)
	}

	// The same id with different content is a conflict, not a silent overwrite.
	conflict := send("mut-1", `"off"`)
	if conflict.Code != http.StatusConflict {
		t.Errorf("reused id with new content = %d, want 409", conflict.Code)
	}

	// The stored value is still the first write's.
	value, err := store.GetSettingValue(context.Background(), userstore.SettingIdentity{
		Key:       "playback.subtitle_mode",
		Scope:     settingscontract.ScopeProfile,
		ProfileID: "profile-1",
	})
	if err != nil || value == nil {
		t.Fatalf("reading stored value: %v", err)
	}
	if string(value.Value) != `"always"` {
		t.Errorf("stored value = %s, want the first write preserved", value.Value)
	}
}

// failingUpsertStore simulates the store failing the write itself — the
// PostgreSQL profile FK rejecting a row, a dropped connection — while every
// other operation, the receipt lookup and insert included, works.
type failingUpsertStore struct {
	userstore.UserStore
}

type failingUpsertMutationWriter struct {
	userstore.SettingMutationWriter
}

func (failingUpsertStore) UpsertSettingValue(
	context.Context, userstore.SettingIdentity, json.RawMessage,
) (*userstore.SettingValue, error) {
	return nil, errors.New("simulated write failure")
}

func (s failingUpsertStore) WithSettingMutationTransaction(
	ctx context.Context,
	mutationID string,
	fn func(userstore.SettingMutationWriter) error,
) error {
	transactioner := s.UserStore.(userstore.SettingMutationTransactioner)
	return transactioner.WithSettingMutationTransaction(ctx, mutationID,
		func(writer userstore.SettingMutationWriter) error {
			return fn(failingUpsertMutationWriter{SettingMutationWriter: writer})
		})
}

func (failingUpsertMutationWriter) UpsertSettingValue(
	context.Context, userstore.SettingIdentity, json.RawMessage,
) (*userstore.SettingValue, error) {
	return nil, errors.New("simulated write failure")
}

// TestFailedWritesLeaveNoReceipt: a receipt for a write that never landed
// would turn the client's retry of a 500 into a silent success replay.
func TestFailedWritesLeaveNoReceipt(t *testing.T) {
	handler, store := newValuesTestHandler(t)
	handler.storeProvider = testUserStoreProvider{store: failingUpsertStore{UserStore: store}}

	send := func() *httptest.ResponseRecorder {
		req := valuesRequest(http.MethodPut,
			"/settings/values/playback.subtitle_mode?scope=profile",
			[]byte(`{"value":"always"}`))
		req.Header.Set(mutationIDHeader, "mut-fail")
		routeCtx := chi.NewRouteContext()
		routeCtx.URLParams.Add("key", "playback.subtitle_mode")
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, routeCtx))
		rec := httptest.NewRecorder()
		handler.HandleSetValue(rec, req)
		return rec
	}

	if rec := send(); rec.Code != http.StatusInternalServerError {
		t.Fatalf("failed write = %d, want 500", rec.Code)
	}
	prior, err := store.GetSettingMutation(context.Background(), "mut-fail")
	if err != nil {
		t.Fatalf("reading mutation receipt: %v", err)
	}
	if prior != nil {
		t.Error("a failed write left an idempotency receipt; its retry would replay a success")
	}
	// And the retry actually retries: with the store healthy again it stores
	// the value rather than replaying a phantom result.
	handler.storeProvider = testUserStoreProvider{store: store}
	retry := send()
	if retry.Code != http.StatusOK {
		t.Fatalf("retry after failure = %d: %s", retry.Code, retry.Body.String())
	}
	if retry.Header().Get("X-Vio-Idempotent-Replay") == "true" {
		t.Error("the retry was served as a replay of the failed attempt")
	}
}

=======
>>>>>>> upstream/main
// storedDeviceIDFor reports the device a profile_device row was written for, or
// "" when the key has no device-scoped row at all. The device-widening tests
// assert on stored rows rather than status codes: before the query parameter is
// honored a named device is silently ignored and the write lands on the header
// device, which is a 200 either way.
func storedDeviceIDFor(t *testing.T, store userstore.UserStore, key string) string {
	t.Helper()
	values, err := store.ListAllSettingValues(context.Background())
	if err != nil {
		t.Fatalf("listing stored values: %v", err)
	}
	for _, value := range values {
		if value.Key == key && value.Scope == settingscontract.ScopeProfileDevice {
			return value.DeviceID
		}
	}
	return ""
}

// --- Household widening: a primary profile addressing a sibling profile ---

// newHouseholdValuesHandler builds a handler with two profiles on one account:
// "profile-1" is the household parent, "profile-2" is another member.
func newHouseholdValuesHandler(t *testing.T, pin string) (*SettingValuesHandler, userstore.UserStore) {
	t.Helper()
	handler, store := newValuesTestHandler(t)

	// profile-1 is already the household parent: is_primary is assigned to the
	// first profile an account creates and is not settable afterwards.
	ctx := context.Background()
	if err := store.CreateProfile(ctx, userstore.Profile{ID: "profile-2", Name: "Robin"}); err != nil {
		t.Fatalf("create sibling: %v", err)
	}
	primary, err := store.GetProfile(ctx, "profile-1")
	if err != nil || primary == nil || !primary.IsPrimary {
		t.Fatalf("profile-1 is not the primary profile (%+v, %v)", primary, err)
	}
	if pin != "" {
		if err := store.UpdateProfile(ctx, "profile-1", userstore.UpdateProfileInput{
			PINHash: storedPIN(t, pin),
		}); err != nil {
			t.Fatalf("set pin: %v", err)
		}
	}

	registry := store.(userstore.DeviceRegistry)
	if err := registry.RegisterDevice(ctx, userstore.DeviceEntry{
		ProfileID: "profile-2", DeviceID: "robin-ipad", DeviceName: "Robin's iPad",
	}); err != nil {
		t.Fatalf("registering sibling device: %v", err)
	}

	return handler, store
}

// captureAuditLogs swaps the default slog handler for the duration of a test.
// The returned function parses whatever has been emitted so far and keeps only
// the settings-audit records.
func captureAuditLogs(t *testing.T) func() []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	return func() []map[string]any {
		var records []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			if line == "" {
				continue
			}
			var record map[string]any
			if err := json.Unmarshal([]byte(line), &record); err != nil {
				continue
			}
			if record["msg"] == settingsAuditMsg {
				records = append(records, record)
			}
		}
		return records
	}
}

func TestMergeLanguageSuggestionsKeepsFloorObservedAndCurrent(t *testing.T) {
	got := mergeLanguageSuggestions(
		[]string{"en", "fr", "pt"},
		[]string{"eng", "deu", "not a tag", "fr"},
		json.RawMessage(`"pt-BR"`),
	)
	want := []string{"en", "fr", "pt", "de", "pt-BR"}
	if !slices.Equal(got, want) {
		t.Errorf("mergeLanguageSuggestions() = %v, want %v", got, want)
	}
}

func TestMergeLanguageSuggestionsUsesExactCurrentAlias(t *testing.T) {
	got := mergeLanguageSuggestions(
		[]string{"en", "fr"},
		[]string{"eng", "fra"},
		json.RawMessage(`"eng"`),
	)
	want := []string{"en", "fr"}
	if !slices.Equal(got, want) {
		t.Errorf("mergeLanguageSuggestions() = %v, want %v", got, want)
	}
}

type recordingLanguageSuggestionSource struct {
	filters  catalog.BrowseFilters
	original []string
	calls    int
}

func (s *recordingLanguageSuggestionSource) ListOriginalLanguages(
	_ context.Context, filters catalog.BrowseFilters,
) ([]string, error) {
	s.filters = filters
	s.calls++
	return s.original, nil
}

func TestObservedLanguageSuggestionsIncludesAccessibleOriginalLanguages(t *testing.T) {
	source := &recordingLanguageSuggestionSource{original: []string{"is", "no"}}
	handler, _ := newValuesTestHandler(t)
	handler.SetLanguageSuggestionSource(source)

	req := valuesRequest(http.MethodGet, "/settings/values/effective", nil)
	req = req.WithContext(access.SetScope(req.Context(), access.Scope{
		AllowedLibraryIDs:  []int{4, 9},
		DisabledLibraryIDs: []int{12},
		MaturityLimits:     access.MaturityLimits{MaxContentRating: "PG-13"},
	}))
	observed := handler.observedLanguageSuggestions(req.Context(), []settingsresolve.Effective{{
		Key: settingskeys.CatalogMetadataLanguage,
	}})

	if !slices.Equal(observed[settingskeys.CatalogMetadataLanguage], []string{"is", "no"}) {
		t.Fatalf("metadata suggestions = %v", observed[settingskeys.CatalogMetadataLanguage])
	}
	if !slices.Equal(source.filters.LibraryIDs, []int{4, 9}) ||
		!slices.Equal(source.filters.DisabledLibraryIDs, []int{12}) ||
		source.filters.MaxContentRating != "PG-13" {
		t.Fatalf("catalog filters = %#v", source.filters)
	}
}

// TestObservedLanguageSuggestionsSkipsPlaybackKeys pins the design decision
// that only catalog.metadata_language gets deployment-observed suggestions.
// The audio/subtitle track listings walk every media file — tens of seconds
// on large catalogs — so those pickers ship the contract floor and clients
// offer free entry for anything beyond it.
func TestObservedLanguageSuggestionsSkipsPlaybackKeys(t *testing.T) {
	source := &recordingLanguageSuggestionSource{original: []string{"is"}}
	handler, _ := newValuesTestHandler(t)
	handler.SetLanguageSuggestionSource(source)

	req := valuesRequest(http.MethodGet, "/settings/values/effective", nil)
	observed := handler.observedLanguageSuggestions(req.Context(), []settingsresolve.Effective{
		{Key: settingskeys.PlaybackAudioLanguage},
		{Key: settingskeys.PlaybackSubtitleLanguage},
	})

	if len(observed) != 0 {
		t.Fatalf("observed suggestions for playback keys = %v, want none", observed)
	}
	if source.calls != 0 {
		t.Fatalf("catalog scans = %d, want 0 for playback-only requests", source.calls)
	}
}

// TestV2SettingReadsBoundRepeatedValueBytes: v2 answers a repeated key once
// per mention, and the batch once per context too, so a large stored document
// must not be copied into one response without limit.
func TestV2SettingReadsBoundRepeatedValueBytes(t *testing.T) {
	handler, store := newValuesTestHandler(t)
	ctx := t.Context()
	key := settingskeys.UiLibraryPageState
	// A schema-valid 128 KiB document: 32 libraries at the search length bound.
	libraries := make(map[string]map[string]string, 32)
	for i := range 32 {
		libraries[strconv.Itoa(i+1)] = map[string]string{"search": strings.Repeat("a", 4096)}
	}
	doc, err := json.Marshal(map[string]any{"version": 1, "libraries": libraries})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertSettingValue(ctx, userstore.SettingIdentity{
		Key: key, Scope: settingscontract.ScopeProfileDevice, ProfileID: "profile-1", DeviceID: "device-1",
	}, doc); err != nil {
		t.Fatalf("seeding document: %v", err)
	}
	device := NewDeviceMetadata("device-1", "", "")
	contexts := make([]EffectiveContextRequest, 100)
	for i := range contexts {
		contexts[i] = EffectiveContextRequest{ContextID: strconv.Itoa(i), SeriesID: "s" + strconv.Itoa(i)}
	}
	batch := func(repeats int) error {
		_, err := handler.ResolveEffectiveSettingContexts(ctx, 1, EffectiveSettingsQuery{
			Keys: slices.Repeat([]string{key}, repeats), ActiveProfileID: "profile-1", Device: device,
		}, contexts)
		return err
	}
	list := func(repeats int) error {
		_, err := handler.ListSettingValues(ctx, 1, slices.Repeat([]string{key}, repeats), SettingIdentityRequest{
			Scope: string(settingscontract.ScopeProfileDevice), ActiveProfileID: "profile-1", Device: device,
		})
		return err
	}

	for name, tc := range map[string]struct {
		read     func(repeats int) error
		repeats  int
		rejected bool
	}{
		"batch within budget":         {read: batch, repeats: 1},
		"batch repeating past budget": {read: batch, repeats: 200, rejected: true},
		"list within budget":          {read: list, repeats: 100},
		"list repeating past budget":  {read: list, repeats: 200, rejected: true},
	} {
		t.Run(name, func(t *testing.T) {
			err := tc.read(tc.repeats)
			var apiErr *APIError
			rejected := errors.As(err, &apiErr) && apiErr.Status == http.StatusBadRequest && apiErr.Field == settingFieldKeys
			if rejected != tc.rejected || (!rejected && err != nil) {
				t.Fatalf("err = %v, want rejected on keys = %v", err, tc.rejected)
			}
		})
	}
}

func TestNavigationShortcutMutationLifecycle(t *testing.T) {
	handler, _ := newValuesTestHandler(t)

	response, document := mustSetShortcut(t, handler, `{"type":"library","library_id":42,"label":"Movies"}`, true)
	if response.Key != settingskeys.NavShortcuts || response.Scope != "profile" || response.Revision != 1 {
		t.Fatalf("add response = %+v, want profile nav.shortcuts revision 1", response)
	}
	if len(document.Items) != 1 || document.Items[0].Label != "Movies" {
		t.Fatalf("add document = %+v, want Movies", document)
	}

	response, document = mustSetShortcut(t, handler, `{"type":"section","library_id":42,"section_id":"recent","label":"Recent"}`, true)
	if response.Revision != 2 || len(document.Items) != 2 || document.Items[1].SectionID != "recent" {
		t.Fatalf("add section = revision %d document %+v, want both items at revision 2", response.Revision, document)
	}

	refresh := `{"type":"library","library_id":42,"label":"Films"}`
	response, document = mustSetShortcut(t, handler, refresh, true)
	if response.Revision != 3 || len(document.Items) != 2 || document.Items[0].Label != "Films" || document.Items[1].SectionID != "recent" {
		t.Fatalf("label refresh reordered or dropped items: revision %d document %+v", response.Revision, document)
	}

	// Exact desired state is a no-op: no revision churn and no duplicate.
	response, document = mustSetShortcut(t, handler, refresh, true)
	if response.Revision != 3 || len(document.Items) != 2 {
		t.Fatalf("repeat add = revision %d document %+v, want unchanged revision 3", response.Revision, document)
	}

	// Removal matches semantic identity only; a stale label cannot stop it.
	remove := `{"type":"library","library_id":42,"label":"Old label"}`
	response, document = mustSetShortcut(t, handler, remove, false)
	if response.Revision != 4 || len(document.Items) != 1 || document.Items[0].SectionID != "recent" {
		t.Fatalf("remove library = revision %d document %+v, want section preserved", response.Revision, document)
	}
	if response, _ = mustSetShortcut(t, handler, remove, false); response.Revision != 4 {
		t.Fatalf("repeat remove revision = %d, want unchanged 4", response.Revision)
	}
}

func TestNavigationShortcutCollectionLibraryIDValidationCannotMutateExistingTargets(t *testing.T) {
	handler, store := newValuesTestHandler(t)
	mustSetShortcut(t, handler, `{"type":"collection","collection_id":"favorites","label":"All Favorites"}`, true)
	mustSetShortcut(t, handler, `{"type":"collection","library_id":42,"collection_id":"favorites","label":"Movie Favorites"}`, true)

	for name, libraryID := range map[string]string{
		"explicit null": `null`,
		"string":        `"42"`,
		"zero":          `0`,
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := setShortcut(t, handler, fmt.Sprintf(
				`{"type":"collection","library_id":%s,"collection_id":"favorites","label":"Ignored"}`, libraryID), false)
			if apiErr := requireSettingStatus(t, err, http.StatusBadRequest, "invalid collection library_id"); apiErr.Code != settingErrorInvalidValue {
				t.Fatalf("code = %q, want %q", apiErr.Code, settingErrorInvalidValue)
			}

			stored, err := store.GetSettingValue(context.Background(), navShortcutIdentity())
			if err != nil || stored == nil {
				t.Fatalf("read catalog after rejected remove: value=%+v err=%v", stored, err)
			}
			var document navigationShortcutDocument
			if err := json.Unmarshal(stored.Value, &document); err != nil {
				t.Fatalf("decode catalog after rejected remove: %v", err)
			}
			if stored.Revision != 2 || len(document.Items) != 2 {
				t.Fatalf("rejected remove mutated catalog: revision %d items %+v", stored.Revision, document.Items)
			}
			if document.Items[0].LibraryID != nil {
				t.Fatalf("global collection gained library identity: %+v", document.Items[0])
			}
			if document.Items[1].LibraryID == nil || *document.Items[1].LibraryID != 42 {
				t.Fatalf("library-specific collection changed identity: %+v", document.Items[1])
			}
		})
	}

	response, document := mustSetShortcut(t, handler, `{"type":"collection","collection_id":"favorites","label":"Ignored"}`, false)
	if response.Revision != 3 || len(document.Items) != 1 || document.Items[0].LibraryID == nil || *document.Items[0].LibraryID != 42 {
		t.Fatalf("global removal = revision %d document %+v, want scoped collection preserved", response.Revision, document)
	}
}

func TestNavigationShortcutMutationValidationAndWholeDocumentGuard(t *testing.T) {
	handler, store := newValuesTestHandler(t)
	ctx := seamCtx(t, "profile-1")

	_, err := handler.SetSettingValue(ctx, seamUserID,
		seamReq(settingskeys.NavShortcuts, settingscontract.ScopeProfile), json.RawMessage(`{"items":[]}`))
	if apiErr := requireSettingStatus(t, err, http.StatusBadRequest, "whole-document write"); apiErr.Code != settingErrorAtomicRequired {
		t.Fatalf("code = %q, want %q", apiErr.Code, settingErrorAtomicRequired)
	}
	if value, err := store.GetSettingValue(context.Background(), navShortcutIdentity()); err != nil || value != nil {
		t.Fatalf("guarded write stored %+v, err=%v", value, err)
	}

	for _, item := range []string{
		`{"type":"builtin","destination":"home","label":"Home"}`,
		`{"type":"library","library_id":42,"label":"Movies","extra":true}`,
		`{"type":"section","library_id":42,"section_id":"","label":"Recent"}`,
		`{"type":"collection","collection_id":"favorites","label":"   "}`,
		`{"type":"library","library_id":0,"label":"Movies"}`,
	} {
		if _, _, err := setShortcut(t, handler, item, true); err == nil {
			t.Errorf("invalid item %s was accepted", item)
		} else {
			requireSettingStatus(t, err, http.StatusBadRequest, "invalid item "+item)
		}
	}
}

func TestNavigationShortcutWholeDocumentDeleteCannotCreateRevisionABA(t *testing.T) {
	handler, store := newValuesTestHandler(t)
	if first, _ := mustSetShortcut(t, handler, `{"type":"library","library_id":42,"label":"Movies"}`, true); first.Revision != 1 {
		t.Fatalf("seed revision = %d, want 1", first.Revision)
	}
	identity := navShortcutIdentity()
	beforeDelete, err := store.GetSettingValue(context.Background(), identity)
	if err != nil || beforeDelete == nil {
		t.Fatalf("read shortcut before guarded delete: value=%+v err=%v", beforeDelete, err)
	}

	err = handler.DeleteSettingValue(seamCtx(t, "profile-1"), seamUserID,
		seamReq(settingskeys.NavShortcuts, settingscontract.ScopeProfile))
	if apiErr := requireSettingStatus(t, err, http.StatusBadRequest, "whole-document delete"); apiErr.Code != settingErrorAtomicRequired {
		t.Fatalf("code = %q, want %q", apiErr.Code, settingErrorAtomicRequired)
	}

	if second, document := mustSetShortcut(t, handler, `{"type":"library","library_id":99,"label":"Shows"}`, true); second.Revision != 2 || len(document.Items) != 2 {
		t.Fatalf("second shortcut = revision %d document %+v", second.Revision, document)
	}

	// Had the delete removed and recreated the row, its revision would return
	// to one and this stale writer could pass an ABA-shaped precondition.
	cas := store.(userstore.SettingValueCompareAndSetter)
	if _, err := cas.CompareAndSetSettingValue(context.Background(), identity,
		beforeDelete.Value, beforeDelete.Revision); !errors.Is(err, userstore.ErrSettingValueRevisionConflict) {
		t.Fatalf("stale CAS after guarded delete error = %v, want revision conflict", err)
	}
}

func TestNavigationShortcutMutationEnforcesDocumentCap(t *testing.T) {
	handler, store := newValuesTestHandler(t)
	items := make([]navigationShortcutItem, 0, 256)
	for id := 1; id <= 256; id++ {
		libraryID := id
		items = append(items, navigationShortcutItem{Type: "library", LibraryID: &libraryID, Label: fmt.Sprintf("Library %d", id)})
	}
	raw, err := json.Marshal(navigationShortcutDocument{Items: items})
	if err != nil {
		t.Fatal(err)
	}
	def, _ := handler.contract.Lookup(settingskeys.NavShortcuts)
	normalized, err := def.ValueSchema.NormalizeValue(raw, settingscontract.ObjectSchemas())
	if err != nil {
		t.Fatalf("normalize seed: %v", err)
	}
	seed, err := store.UpsertSettingValue(context.Background(), navShortcutIdentity(), normalized)
	if err != nil {
		t.Fatalf("seed full catalog: %v", err)
	}

	_, _, err = setShortcut(t, handler, `{"type":"library","library_id":257,"label":"Too Many"}`, true)
	if apiErr := requireSettingStatus(t, err, http.StatusBadRequest, "257th shortcut"); apiErr.Code != settingErrorInvalidValue {
		t.Fatalf("code = %q, want %q", apiErr.Code, settingErrorInvalidValue)
	}
	got, err := store.GetSettingValue(context.Background(), navShortcutIdentity())
	if err != nil || got == nil || got.Revision != seed.Revision {
		t.Fatalf("catalog after rejected add = %+v, err=%v; want unchanged revision %d", got, err, seed.Revision)
	}
}

func TestNavigationShortcutConcurrentAddsMerge(t *testing.T) {
	baseHandler, baseStore := newValuesTestHandler(t)
	wrapped := &synchronizedShortcutStore{
		UserStore: baseStore,
		cas:       baseStore.(userstore.SettingValueCompareAndSetter),
		ready:     make(chan struct{}),
	}
	handler := NewSettingValuesHandler(testUserStoreProvider{store: wrapped}, baseHandler.contract)
	items := []string{
		`{"type":"library","library_id":42,"label":"Movies"}`,
		`{"type":"library","library_id":99,"label":"Shows"}`,
	}

	errs := make(chan error, len(items))
	for _, item := range items {
		go func() {
			_, err := handler.SetNavigationShortcut(t.Context(), seamUserID, "profile-1", json.RawMessage(item), true)
			errs <- err
		}()
	}
	for range items {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent add: %v", err)
		}
	}

	got, err := baseStore.GetSettingValue(context.Background(), navShortcutIdentity())
	if err != nil || got == nil {
		t.Fatalf("read merged catalog: value=%+v err=%v", got, err)
	}
	var document navigationShortcutDocument
	if err := json.Unmarshal(got.Value, &document); err != nil {
		t.Fatalf("decode merged catalog: %v", err)
	}
	if got.Revision != 2 || len(document.Items) != 2 {
		t.Fatalf("merged catalog = revision %d items %+v, want both adds at revision 2", got.Revision, document.Items)
	}
}

func TestSettingValuesRejectNonexistentLibraryContext(t *testing.T) {
	handler, store := newValuesTestHandler(t)
	handler.SetLibraryLookup(settingValuesLibraryLookup{existing: map[int]bool{7: true}})
	ctx := seamCtx(t, "profile-1")
	req := seamReq("playback.subtitle_language", settingscontract.ScopeProfileLibrary)
	req.LibraryID = "99"

	_, err := handler.GetSettingValue(ctx, seamUserID, req)
	requireSettingStatus(t, err, http.StatusNotFound, "read at a nonexistent library")
	_, err = handler.SetSettingValue(ctx, seamUserID, req, json.RawMessage(`"de"`))
	requireSettingStatus(t, err, http.StatusNotFound, "write at a nonexistent library")
	err = handler.DeleteSettingValue(ctx, seamUserID, req)
	requireSettingStatus(t, err, http.StatusNotFound, "clear at a nonexistent library")

	value, err := store.GetSettingValue(context.Background(), userstore.SettingIdentity{
		Key: "playback.subtitle_language", Scope: settingscontract.ScopeProfileLibrary,
		ProfileID: "profile-1", LibraryID: 99,
	})
	if err != nil || value != nil {
		t.Fatalf("nonexistent library left value (%+v, %v)", value, err)
	}

	req.LibraryID = "7"
	seamSet(t, ctx, handler, req, `"de"`)
}

// TestSettingWritesAreRefusedOutsideTheContract: the contract decides which
// keys the server stores, at which scopes, and in what shape. A refused write
// stores nothing.
func TestSettingWritesAreRefusedOutsideTheContract(t *testing.T) {
	for name, tc := range map[string]struct {
		key, value string
		scope      settingscontract.Scope
		seriesID   string
		libraryID  string
		wantStatus int
	}{
		// The legacy endpoint stored any unregistered key as an unvalidated
		// string, which is how untyped keys reached production.
		"unknown key":                         {key: "totally.invented.key", value: `"x"`, scope: settingscontract.ScopeProfile, wantStatus: http.StatusNotFound},
		"client-local key":                    {key: "downloads.wifi_only", value: `true`, scope: settingscontract.ScopeProfile, wantStatus: http.StatusBadRequest},
		"scope the definition does not allow": {key: "ui.custom_css", value: `"body{}"`, scope: settingscontract.ScopeProfileDevice, wantStatus: http.StatusBadRequest},
		"missing scope":                       {key: "playback.subtitle_mode", value: `"always"`, wantStatus: http.StatusBadRequest},
		"library scope without library_id":    {key: "playback.subtitle_language", value: `"de"`, scope: settingscontract.ScopeProfileLibrary, wantStatus: http.StatusBadRequest},
		"series scope without series_id":      {key: "playback.subtitle_language", value: `"de"`, scope: settingscontract.ScopeProfileSeries, wantStatus: http.StatusBadRequest},
		"value outside the schema":            {key: "playback.subtitle_mode", value: `"sideways"`, scope: settingscontract.ScopeProfile, wantStatus: http.StatusBadRequest},
		"library write":                       {key: "playback.subtitle_language", value: `"de"`, scope: settingscontract.ScopeProfileLibrary, libraryID: "7"},
		"series write":                        {key: "playback.subtitle_language", value: `"ja"`, scope: settingscontract.ScopeProfileSeries, seriesID: "s1"},
	} {
		t.Run(name, func(t *testing.T) {
			handler, store := newValuesTestHandler(t)
			req := seamReq(tc.key, tc.scope)
			req.LibraryID, req.SeriesID = tc.libraryID, tc.seriesID
			_, err := handler.SetSettingValue(seamCtx(t, "profile-1"), seamUserID, req, json.RawMessage(tc.value))
			if tc.wantStatus == 0 {
				if err != nil {
					t.Fatalf("write: %v", err)
				}
				return
			}
			requireSettingStatus(t, err, tc.wantStatus, "refused write")
			if values, err := store.ListAllSettingValues(context.Background()); err != nil || len(values) != 0 {
				t.Fatalf("refused write stored %+v (%v)", values, err)
			}
		})
	}
}

// TestEffectiveResolutionRefusals: an effective read fails closed instead of
// returning a plausible wrong answer. An unknown key would let a client fill
// the gap with its own vendored default, and a device-aware key resolved
// without a device would pass the profile fallback off as effective.
func TestEffectiveResolutionRefusals(t *testing.T) {
	handler, _ := newValuesTestHandler(t)
	ctx := seamCtx(t, "profile-1")
	noDevice := func(keys ...string) EffectiveSettingsQuery {
		q := seamQuery(keys...)
		q.Device = DeviceMetadata{}
		return q
	}
	badFamily := seamQuery(settingskeys.UiCardPresentation)
	badFamily.ClientFamily = "TV"

	for name, tc := range map[string]struct {
		query      EffectiveSettingsQuery
		wantStatus int
	}{
		"unknown key":                     {query: seamQuery("playback.subtitle_mode", "totally.invented.key"), wantStatus: http.StatusNotFound},
		"device-aware key without device": {query: noDevice("playback.subtitle_language"), wantStatus: http.StatusBadRequest},
		"all keys without device":         {query: noDevice(), wantStatus: http.StatusBadRequest},
		"invalid client family":           {query: badFamily, wantStatus: http.StatusBadRequest},
		"profile-only key without device": {query: noDevice("ui.custom_css")},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := handler.ResolveEffectiveSettings(ctx, seamUserID, tc.query)
			if tc.wantStatus == 0 {
				if err != nil {
					t.Fatalf("resolve: %v", err)
				}
				return
			}
			apiErr := requireSettingStatus(t, err, tc.wantStatus, "refused resolve")
			if tc.wantStatus == http.StatusNotFound && !strings.Contains(apiErr.Message, "totally.invented.key") {
				t.Errorf("the error does not name the offending key: %s", apiErr.Message)
			}
		})
	}
}

func TestPostEffectiveRejectsInvalidContexts(t *testing.T) {
	handler, _ := newValuesTestHandler(t)
	for name, contexts := range map[string][]EffectiveContextRequest{
		"empty":           {},
		"duplicate id":    {{ContextID: "x", SeriesID: "s1"}, {ContextID: "x", SeriesID: "s2"}},
		"missing content": {{ContextID: "x"}},
		"invalid library": {{ContextID: "x", LibraryID: json.RawMessage(`"nope"`)}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := handler.ResolveEffectiveSettingContexts(seamCtx(t, "profile-1"), seamUserID,
				seamQuery("ui.custom_css"), contexts)
			requireSettingStatus(t, err, http.StatusBadRequest, "invalid contexts")
		})
	}
}

// TestEffectiveClientFamilyIsOptionalButValidated: an absent family drops the
// profile_client layer so older callers keep resolving broader fallbacks.
func TestEffectiveClientFamilyIsOptionalButValidated(t *testing.T) {
	handler, _ := newValuesTestHandler(t)
	ctx := seamCtx(t, "profile-1")
	key := settingskeys.UiCardPresentation
	seamSet(t, ctx, handler, seamReq(key, settingscontract.ScopeProfile), `{"poster_size":"compact","caption":"title"}`)
	seamSet(t, ctx, handler, seamReq(key, settingscontract.ScopeProfileClient), `{"poster_size":"large","caption":"artwork"}`)

	absent := seamQuery(key)
	absent.ClientFamily = ""
	if got := seamEffective(t, ctx, handler, absent); got.Source != string(settingscontract.ScopeProfile) ||
		string(got.Value) != `{"poster_size":"compact","caption":"title"}` {
		t.Fatalf("effective without family = %s from %s, want profile fallback", got.Value, got.Source)
	}
	all := seamQuery()
	all.ClientFamily = ""
	if _, err := handler.ResolveEffectiveSettings(ctx, seamUserID, all); err != nil {
		t.Fatalf("resolve all without client family: %v", err)
	}
	if got := seamEffective(t, ctx, handler, seamQuery(key)); got.Source != string(settingscontract.ScopeProfileClient) || got.ClientFamily != "web" {
		t.Fatalf("effective with family = %s/%s, want profile_client/web", got.Source, got.ClientFamily)
	}
}

// TestEffectiveAppliesViewerQualityCap: the access scope's MaxPlaybackQuality
// becomes the resolver's ceiling, so a capped viewer resolves the cap, and the
// authored preference survives untouched to take effect when the cap lifts.
func TestEffectiveAppliesViewerQualityCap(t *testing.T) {
	handler, store := newValuesTestHandler(t)
	ctx := seamCtx(t, "profile-1")
	seamSet(t, ctx, handler, seamReq("playback.preferred_quality", settingscontract.ScopeProfile), `"2160p"`)

	effective := func(maxQuality string) EffectiveSettingValueView {
		t.Helper()
		scoped := access.SetScope(ctx, access.Scope{UserID: 1, ProfileID: "profile-1", MaxPlaybackQuality: maxQuality})
		return seamEffective(t, scoped, handler, seamQuery("playback.preferred_quality"))
	}

	got := effective("1080p")
	if string(got.Value) != `"1080p"` || !got.Constrained || got.ConstraintKind != string(settingscontract.ConstraintCeiling) {
		t.Errorf("capped effective = %s constrained=%v kind=%q, want \"1080p\"/true/ceiling",
			got.Value, got.Constrained, got.ConstraintKind)
	}
	if string(got.StoredValue) != `"2160p"` || string(got.RequestedValue) != `"2160p"` {
		t.Errorf("stored/requested = %s/%s, want the authored \"2160p\"", got.StoredValue, got.RequestedValue)
	}
	if got.ConstrainedBy == nil || got.ConstrainedBy.PolicyInput != policyInputMaxPlaybackQuality {
		t.Errorf("constrained_by = %#v", got.ConstrainedBy)
	}

	stored, err := store.GetSettingValue(context.Background(), userstore.SettingIdentity{
		Key: "playback.preferred_quality", Scope: settingscontract.ScopeProfile, ProfileID: "profile-1",
	})
	if err != nil || stored == nil || string(stored.Value) != `"2160p"` {
		t.Fatalf("stored row = %+v (%v), want \"2160p\" untouched", stored, err)
	}

	if got = effective(""); string(got.Value) != `"2160p"` || got.Constrained || got.StoredValue != nil {
		t.Errorf("uncapped effective = %s constrained=%v stored=%s, want \"2160p\" unconstrained",
			got.Value, got.Constrained, got.StoredValue)
	}
}

// TestSettingAuthorityForOtherProfilesAndDevices: a request may name another
// profile only as the household parent, and another device only when it
// belongs to the addressed profile. Unknown ids answer 404 so the caller
// learns nothing about ids elsewhere.
func TestSettingAuthorityForOtherProfilesAndDevices(t *testing.T) {
	type call int
	const (
		set call = iota
		clearValue
		resolve
	)
	for name, tc := range map[string]struct {
		pin        string
		acting     string
		admin      bool
		profileID  string
		deviceID   string
		op         call
		wantStatus int
		wantMsg    string
		// wantRow is the (profile, device) a successful write lands on.
		wantRow [2]string
	}{
		"write to a device the profile does not own":    {acting: "profile-1", deviceID: "device-someone-else", wantStatus: http.StatusNotFound},
		"clear on a device the profile does not own":    {acting: "profile-1", deviceID: "device-someone-else", op: clearValue, wantStatus: http.StatusNotFound},
		"resolve a device the profile does not own":     {acting: "profile-1", deviceID: "not-mine", op: resolve, wantStatus: http.StatusNotFound},
		"non-primary names a sibling":                   {acting: "profile-2", profileID: "profile-1", deviceID: "device-1", wantStatus: http.StatusForbidden},
		"non-primary resolves a sibling":                {acting: "profile-2", profileID: "profile-1", op: resolve, wantStatus: http.StatusForbidden},
		"non-primary on an admin account":               {acting: "profile-2", admin: true, profileID: "profile-1", deviceID: "device-1", wantStatus: http.StatusForbidden},
		"primary with an unverified PIN":                {pin: "1234", acting: "profile-1", profileID: "profile-2", deviceID: "robin-ipad", wantStatus: http.StatusForbidden, wantMsg: "PIN"},
		"profile from another account":                  {acting: "profile-1", profileID: "someone-elses-profile", deviceID: "device-1", wantStatus: http.StatusNotFound},
		"primary names a sibling with a foreign device": {acting: "profile-1", profileID: "profile-2", deviceID: "not-robins-device", wantStatus: http.StatusNotFound},
		"primary writes a sibling's device":             {acting: "profile-1", profileID: "profile-2", deviceID: "robin-ipad", wantRow: [2]string{"profile-2", "robin-ipad"}},
		"anyone may name their own profile":             {acting: "profile-2", profileID: "profile-2", deviceID: "robin-ipad", wantRow: [2]string{"profile-2", "robin-ipad"}},
	} {
		t.Run(name, func(t *testing.T) {
			handler, store := newHouseholdValuesHandler(t, tc.pin)
			ctx := seamCtx(t, tc.acting)
			if tc.admin {
				ctx = apimw.SetClaims(ctx, &auth.Claims{UserID: seamUserID, Role: "admin"})
			}
			const key = "player.hdr_enabled"
			req := seamReq(key, settingscontract.ScopeProfileDevice)
			req.ActiveProfileID, req.ProfileID, req.DeviceID = tc.acting, tc.profileID, tc.deviceID

			var err error
			switch tc.op {
			case set:
				_, err = handler.SetSettingValue(ctx, seamUserID, req, json.RawMessage(`false`))
			case clearValue:
				err = handler.DeleteSettingValue(ctx, seamUserID, req)
			case resolve:
				q := seamQuery("playback.subtitle_mode")
				q.ActiveProfileID, q.ProfileID, q.DeviceID = tc.acting, tc.profileID, tc.deviceID
				_, err = handler.ResolveEffectiveSettings(ctx, seamUserID, q)
			}

			if tc.wantStatus != 0 {
				apiErr := requireSettingStatus(t, err, tc.wantStatus, "refused call")
				if !strings.Contains(apiErr.Message, tc.wantMsg) {
					t.Errorf("message %q does not mention %q", apiErr.Message, tc.wantMsg)
				}
				if got := storedDeviceIDFor(t, store, key); got != "" {
					t.Errorf("refused call wrote a row on device %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			values, err := store.ListAllSettingValues(context.Background())
			if err != nil || len(values) != 1 || values[0].ProfileID != tc.wantRow[0] || values[0].DeviceID != tc.wantRow[1] {
				t.Fatalf("stored rows = %+v (%v), want one on %v", values, err, tc.wantRow)
			}
		})
	}
}

// TestSettingWritesRegisterOnlyTheCallersOwnDevice: registration means "this
// device is in use by this profile". A write aimed at another device, or made
// on another profile's behalf, must not file the actor's device anywhere.
func TestSettingWritesRegisterOnlyTheCallersOwnDevice(t *testing.T) {
	for name, tc := range map[string]struct {
		profileID, deviceID string
		checkProfile        string
	}{
		"another device of the same profile": {deviceID: "apple-tv", checkProfile: "profile-1"},
		"a sibling profile's device":         {profileID: "profile-2", deviceID: "robin-ipad", checkProfile: "profile-2"},
	} {
		t.Run(name, func(t *testing.T) {
			handler, store := newHouseholdValuesHandler(t, "")
			registry := store.(userstore.DeviceRegistry)
			if err := registry.RegisterDevice(context.Background(), userstore.DeviceEntry{
				ProfileID: "profile-1", DeviceID: "apple-tv", DeviceName: "Apple TV",
			}); err != nil {
				t.Fatalf("registering apple-tv: %v", err)
			}
			req := seamReq("player.hdr_enabled", settingscontract.ScopeProfileDevice)
			req.ProfileID, req.DeviceID = tc.profileID, tc.deviceID
			seamSet(t, seamCtx(t, "profile-1"), handler, req, `false`)

			exists, err := registry.DeviceExists(context.Background(), tc.checkProfile, "device-1")
			if err != nil {
				t.Fatalf("DeviceExists: %v", err)
			}
			if exists {
				t.Errorf("registered the acting device-1 under %s", tc.checkProfile)
			}
		})
	}
}

func TestSetValue_AuditsCrossProfileWrite(t *testing.T) {
	handler, _ := newHouseholdValuesHandler(t, "")
	audited := captureAuditLogs(t)

	req := seamReq("playback.subtitle_mode", settingscontract.ScopeProfileDevice)
	req.ProfileID, req.DeviceID = "profile-2", "robin-ipad"
	seamSet(t, seamCtx(t, "profile-1"), handler, req, `"always"`)

	records := audited()
	if len(records) != 1 {
		t.Fatalf("emitted %d audit records, want 1: %+v", len(records), records)
	}
	record := records[0]
	if record["actor_profile_id"] != "profile-1" || record["target_profile_id"] != "profile-2" {
		t.Errorf("actor/target = %v/%v, want profile-1/profile-2",
			record["actor_profile_id"], record["target_profile_id"])
	}
	if record["setting_key"] != "playback.subtitle_mode" || record["device_id"] != "robin-ipad" {
		t.Errorf("key/device = %v/%v", record["setting_key"], record["device_id"])
	}
	// Identity only: the value must never reach an operator's log.
	for key, value := range record {
		if text, ok := value.(string); ok && text == "always" {
			t.Errorf("audit record leaked the value under %q", key)
		}
	}
}

// TestEffectiveContextsPreserveContextIDsAndRepeatedKeys: v2 echoes each
// context_id exactly as sent and answers a repeated key once per mention.
func TestEffectiveContextsPreserveContextIDsAndRepeatedKeys(t *testing.T) {
	handler, _ := newValuesTestHandler(t)
	keys := []string{"ui.custom_css", "ui.custom_css"}
	contexts := []EffectiveContextRequest{{ContextID: " row-1 ", SeriesID: "s1"}, {ContextID: "row-1", SeriesID: "s2"}}
	views, err := handler.ResolveEffectiveSettingContexts(t.Context(), seamUserID, EffectiveSettingsQuery{
		ActiveProfileID: "profile-1", Keys: keys,
	}, contexts)
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 2 {
		t.Fatalf("contexts = %d", len(views))
	}
	for i, view := range views {
		if view.ContextID != contexts[i].ContextID {
			t.Fatalf("context id = %q, want %q", view.ContextID, contexts[i].ContextID)
		}
		if len(view.Settings) != 2 || view.Settings[0].Key != keys[0] || view.Settings[1].Key != keys[1] {
			t.Fatalf("settings = %+v; want both requested keys in order", view.Settings)
		}
	}
}

func TestGetSettingValuesReportsSetAndUnsetAtOneScope(t *testing.T) {
	handler, store := newValuesTestHandler(t)
	if _, err := store.UpsertSettingValue(context.Background(), userstore.SettingIdentity{
		Key: "playback.subtitle_mode", Scope: settingscontract.ScopeProfile, ProfileID: "profile-1",
	}, json.RawMessage(`"always"`)); err != nil {
		t.Fatalf("seeding explicit value: %v", err)
	}

	views, err := handler.ListSettingValues(seamCtx(t, "profile-1"), seamUserID,
		[]string{"playback.subtitle_mode", "playback.subtitle_language"},
		seamReq("", settingscontract.ScopeProfile))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(views) != 2 {
		t.Fatalf("values = %d, want 2", len(views))
	}
	if views[0].Key != "playback.subtitle_mode" || !views[0].IsSet || string(views[0].Value) != `"always"` {
		t.Errorf("stored entry = %+v", views[0])
	}
	if views[1].Key != "playback.subtitle_language" || views[1].IsSet || views[1].Value != nil {
		t.Errorf("unset entry = %+v", views[1])
	}
}

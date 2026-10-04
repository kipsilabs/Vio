package apiv2

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/downloads"
)

// A client that attaches X-Silo-Device-Id twice reaches Huma as "id,id";
// v1 read the first line and never saw it. The guard refuses the repeated
// header and any comma-bearing or whitespace-bearing value before the
// operation binds it, so no device identity like that can be stored.
func TestDeviceHeaderGuardRefusesRepeatedAndMalformedValues(t *testing.T) {
	svc := &fakeDownloadCreation{row: &downloads.Download{ID: "entry", ContentID: "movie", MediaFileID: 42, Revision: 1, CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}}
	deps := pilotDeps(nil, nil)
	deps.DownloadCreation = svc
	h := newTestHandler(t, deps)
	body := `{"content_id":"movie","media_file_id":"42","expected_revision":0}`
	const id = "ef703653-6a0b-4334-bd14-3a0497e9647d"
	send := func(lines ...string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, Prefix+"/downloads", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+memberToken)
		r.Header.Set("X-Profile-Id", "p-owner")
		for _, line := range lines {
			r.Header.Add("X-Vio-Device-Id", line)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	// One valid line: accepted and stored exactly.
	if rec := send(id); rec.Code != http.StatusAccepted || svc.req.DeviceID != id {
		t.Fatalf("single header: %d %s %q", rec.Code, rec.Body.String(), svc.req.DeviceID)
	}
	svc.req.DeviceID = ""
	// Two header lines (the Android F7 shape): refused before the seam.
	p := requireProblem(t, send(id, id), TypeValidationFailed)
	if len(p.Errors) != 1 || p.Errors[0].Location != locationDeviceHeader || !strings.Contains(p.Errors[0].Detail, "2 times") || svc.req.DeviceID != "" {
		t.Fatalf("duplicate lines: %+v %q", p.Errors, svc.req.DeviceID)
	}
	// One line already carrying the joined value: refused as malformed.
	for _, bad := range []string{id + "," + id, "a b", "id,", ",", "i\td", "id;x", "ïd", " a,b "} {
		p := requireProblem(t, send(bad), TypeValidationFailed)
		if len(p.Errors) != 1 || p.Errors[0].Location != locationDeviceHeader || p.Errors[0].Code != codeInvalid || svc.req.DeviceID != "" {
			t.Fatalf("value %q: %+v %q", bad, p.Errors, svc.req.DeviceID)
		}
	}
	// Shapes clients really send stay accepted: UUID, opaque token, fixtures,
	// and surrounding whitespace, which the seam trims as v1 does.
	for _, good := range []string{id, "fixture-device-b", "iphone-1", "Living.Room_TV:1", strings.Repeat("a", 128), " tablet-2 "} {
		if rec := send(good); rec.Code != http.StatusAccepted || svc.req.DeviceID != strings.TrimSpace(good) {
			t.Fatalf("value %q: %d %s", good, rec.Code, rec.Body.String())
		}
	}
	// The length bound stays the operation's own declared rule and answer.
	p = requireProblem(t, send(strings.Repeat("x", 129)), TypeValidationFailed)
	if len(p.Errors) != 1 || p.Errors[0].Location == locationDeviceHeader || p.Errors[0].Code != "out_of_range" {
		t.Fatalf("too long must stay the operation's rule: %+v", p.Errors)
	}
	// An absent or present-but-empty header is left to the operation's own
	// rule: this revision-guarded create answers its own 400, not the guard's
	// header-location 422.
	for _, lines := range [][]string{nil, {""}} {
		p := requireProblem(t, send(lines...), TypeMalformedRequest)
		for _, e := range p.Errors {
			if e.Location == locationDeviceHeader {
				t.Fatalf("guard intervened on %q: %+v", lines, p.Errors)
			}
		}
	}
}

// The legacy X-Silo-Device-Id spelling is accepted on ingest (see
// rejectMalformedDeviceHeader): a request carrying only the legacy spelling
// has it promoted to X-Vio-Device-Id before the operation binds it.
func TestDeviceHeaderGuardPromotesLegacySpelling(t *testing.T) {
	svc := &fakeDownloadCreation{row: &downloads.Download{ID: "entry", ContentID: "movie", MediaFileID: 42, Revision: 1, CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}}
	deps := pilotDeps(nil, nil)
	deps.DownloadCreation = svc
	h := newTestHandler(t, deps)
	body := `{"content_id":"movie","media_file_id":"42","expected_revision":0}`
	r := httptest.NewRequest(http.MethodPost, Prefix+"/downloads", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+memberToken)
	r.Header.Set("X-Profile-Id", "p-owner")
	r.Header.Set("X-Silo-Device-Id", "legacy-tv-1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusAccepted || svc.req.DeviceID != "legacy-tv-1" {
		t.Fatalf("legacy header: %d %s %q", rec.Code, rec.Body.String(), svc.req.DeviceID)
	}
}

// The guard runs for every v2 operation, including ones that never bind the
// header, and its refusal is a problem document with the request id.
func TestDeviceHeaderGuardCoversEveryOperation(t *testing.T) {
	h := newTestHandler(t, pilotDeps(nil, nil))
	for _, path := range []string{Prefix + "/system/info", Prefix + "/devices", Prefix + "/downloads", Prefix + "/settings/subtitle-appearance/device"} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Authorization", "Bearer "+memberToken)
		r.Header.Set("X-Profile-Id", "p-owner")
		r.Header.Add("X-Vio-Device-Id", "one")
		r.Header.Add("X-Vio-Device-Id", "two")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		requireProblem(t, rec, TypeValidationFailed)
		if requestIDHeader(rec) == "" {
			t.Fatalf("%s: refusal lost the request id", path)
		}
	}
}

// The legacy X-Silo-Client-Family spelling is promoted to X-Vio-Client-Family
// before binding, like the legacy device header. Canonical wins when both are
// present; an invalid legacy value is left for the operation's own enum
// validation.
func TestClientFamilyHeaderPromotesLegacySpelling(t *testing.T) {
	promote := func(headers map[string]string) string {
		rr := httptest.NewRequest(http.MethodGet, Prefix+"/system/info", nil)
		for k, v := range headers {
			rr.Header.Set(k, v)
		}
		rejectMalformedDeviceHeader(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("X-Promoted-Family", req.Header.Get(clientFamilyHeader))
		})).ServeHTTP(httptest.NewRecorder(), rr)
		return rr.Header.Get(clientFamilyHeader)
	}
	if got := promote(map[string]string{"X-Silo-Client-Family": "tv"}); got != "tv" {
		t.Fatalf("legacy-only not promoted: %q", got)
	}
	if got := promote(map[string]string{"X-Vio-Client-Family": "mobile", "X-Silo-Client-Family": "tv"}); got != "mobile" {
		t.Fatalf("canonical must win: %q", got)
	}
	if got := promote(map[string]string{"X-Silo-Client-Family": "not-a-family"}); got != "not-a-family" {
		t.Fatalf("invalid legacy must pass through to operation validation: %q", got)
	}
}

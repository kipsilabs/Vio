package apiv2

import (
	"encoding/json"
	"net/http"
	"testing"
)

// fakeVirtualLibraryStatus is the capability seam.
type fakeVirtualLibraryStatus struct {
	search  bool
	request bool
	prune   bool
	waits   bool
}

func (f fakeVirtualLibraryStatus) IndexerCapabilities() (bool, bool) { return f.search, f.request }
func (f fakeVirtualLibraryStatus) RefreshPrunesDeadCandidates() bool { return f.prune }
func (f fakeVirtualLibraryStatus) WaitForImports() bool              { return f.waits }

func TestVirtualLibraryCapabilities(t *testing.T) {
	type wantCase = struct {
		name    string
		deps    func() Dependencies
		want    string
		search  bool
		request bool
		prune   bool
		waits   bool
	}
	cases := []wantCase{
		{
			name: "unwired is not_configured",
			deps: func() Dependencies { return pilotDeps(nil, nil) },
			want: StateNotConfigured,
		},
		{
			name: "indexer search only",
			deps: func() Dependencies {
				deps := pilotDeps(nil, nil)
				deps.VirtualLibraryStatus = fakeVirtualLibraryStatus{search: true}
				return deps
			},
			want: StateAvailable, search: true,
		},
		{
			name: "both wired",
			deps: func() Dependencies {
				deps := pilotDeps(nil, nil)
				deps.VirtualLibraryStatus = fakeVirtualLibraryStatus{search: true, request: true, prune: true, waits: true}
				return deps
			},
			want: StateAvailable, search: true, request: true, prune: true, waits: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHandler(t, tc.deps())
			rec := do(t, h, http.MethodGet, "/api/v2/capabilities/virtual-library", "", bearer(memberToken))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d; body %s", rec.Code, rec.Body.String())
			}
			var body struct {
				State                       string `json:"state"`
				IndexerSearch               bool   `json:"indexer_search"`
				IndexerRequest              bool   `json:"indexer_request"`
				RefreshPrunesDeadCandidates bool   `json:"refresh_prunes_dead_candidates"`
				WaitForImports              bool   `json:"wait_for_imports"`
				Revision                    string `json:"revision"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.State != tc.want {
				t.Fatalf("state = %q, want %q", body.State, tc.want)
			}
			if body.IndexerSearch != tc.search || body.IndexerRequest != tc.request {
				t.Fatalf("flags = %+v", body)
			}
			if body.RefreshPrunesDeadCandidates != tc.prune {
				t.Fatalf("refresh_prunes_dead_candidates = %v want %v", body.RefreshPrunesDeadCandidates, tc.prune)
			}
			if body.WaitForImports != tc.waits {
				t.Fatalf("wait_for_imports = %v want %v", body.WaitForImports, tc.waits)
			}
			if body.Revision == "" {
				t.Fatal("capability document has no revision")
			}
		})
	}
}

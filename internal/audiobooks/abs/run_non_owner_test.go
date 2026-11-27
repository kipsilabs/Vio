package abs

import (
	"net/http"
	"testing"
)

// runNonOwner404 creates the owner's row in a fresh harness for each case and
// asserts that user 2's request answers 404 without leaking or mutating it.
func runNonOwner404[H any](t *testing.T, cases []nonOwnerCase[H], newHarness func(t *testing.T, known ...string) H, handler func(H) *Handler, base string, create func(*Handler) http.HandlerFunc) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var known []string
			if tc.known != "" {
				known = append(known, tc.known)
			}
			hb := newHarness(t, known...)
			h := handler(hb)
			seed := tc.seed
			if seed == "" {
				seed = `{"name":"mine"}`
			}
			id := createForUser(t, base, "1", "", seed, create(h))
			if tc.before != nil {
				tc.before(t, hb, id)
			}
			params := map[string]string{"id": id}
			for k, v := range tc.params {
				params[k] = v
			}
			var body []byte
			if tc.body != "" {
				body = []byte(tc.body)
			}
			rec := dispatchABSWithParams(tc.method, base+"/"+id+tc.suffix, params, body, "2", "",
				func(w http.ResponseWriter, r *http.Request) { tc.handler(h, w, r) })
			if rec.Code != http.StatusNotFound {
				t.Errorf("non-owner status = %d, want 404 (no leak); body=%s", rec.Code, rec.Body.String())
			}
			if tc.check != nil {
				tc.check(t, hb, id)
			}
		})
	}
}

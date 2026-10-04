package virtuallibrary

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Silo-Server/silo-server/internal/virtuallibrary/quality"
)

// TestServiceValidateConfigSurvivesBadCustomFormat proves the reported
// outage chain stays fixed end to end: activation (ValidateConfig) succeeds
// repeatedly with one uncompilable custom format, the follow-up validation
// inside CheckRemote succeeds too, and strict validation still rejects the
// original bad pattern. The manifest is a controlled httptest stub.
func TestServiceValidateConfigSurvivesBadCustomFormat(t *testing.T) {
	const badPattern = `(?:(?<=^)MULTI)`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"test-addon","resources":["stream"],"types":["movie"]}`))
	}))
	defer server.Close()

	cfg := Config{
		Enabled:           true,
		ManifestURL:       server.URL + "/manifest.json",
		AllowInsecureHTTP: true,
		Quality: quality.QualityConfig{CustomFormats: []quality.CustomFormat{
			{Name: "Good", Pattern: `\b1080p\b`, PatternType: "regex", Enabled: true},
			{Name: "Bad", Pattern: badPattern, PatternType: "regex", Enabled: true},
		}},
	}
	svc := New(cfg, nil, nil)
	if svc == nil {
		t.Fatal("New returned nil for an enabled service with a manifest URL")
	}
	for attempt := 1; attempt <= 2; attempt++ {
		if err := svc.ValidateConfig(); err != nil {
			t.Fatalf("ValidateConfig attempt %d: %v", attempt, err)
		}
	}
	if err := svc.CheckRemote(context.Background()); err != nil {
		t.Fatalf("CheckRemote with controlled provider: %v", err)
	}
	if got := svc.cfg.Quality.CustomFormats[1].EffectivePattern(); got != badPattern {
		t.Fatalf("bad source pattern not preserved: %q", got)
	}
	if err := svc.cfg.Quality.Validate(); err == nil {
		t.Fatal("strict Validate must still reject the bad pattern")
	} else if !strings.Contains(err.Error(), "Bad") {
		t.Fatalf("strict error %q does not name the bad format", err)
	}
}

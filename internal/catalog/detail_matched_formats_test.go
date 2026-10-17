package catalog

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestFileVersionMatchedFormatsSerialization pins the catalog version payload:
// a scored row carries matched_formats alongside format_score, and an unscored
// row omits both.
func TestFileVersionMatchedFormatsSerialization(t *testing.T) {
	scored, err := json.Marshal(FileVersion{
		FileID:         7,
		FormatScore:    intPtr(850),
		MatchedFormats: []string{"English", "Remux Tier 1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(scored, &body); err != nil {
		t.Fatal(err)
	}
	formats, ok := body["matched_formats"].([]any)
	if !ok || len(formats) != 2 || formats[0] != "English" || formats[1] != "Remux Tier 1" {
		t.Fatalf("matched_formats = %v, want [English Remux Tier 1]", body["matched_formats"])
	}

	unscored, err := json.Marshal(FileVersion{FileID: 8})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(unscored), "matched_formats") {
		t.Fatalf("unscored row carried matched_formats: %s", unscored)
	}
}

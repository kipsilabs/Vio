package handlers

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
)

func TestV1LibraryCreateDecodesThresholdOverrides(t *testing.T) {
	var create createLibraryRequest
	if err := json.Unmarshal([]byte(`{"paths":["/m"],"type":"movies","name":"M","watched_threshold":80,"min_resume_threshold":10}`), &create); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if create.WatchedThreshold == nil || *create.WatchedThreshold != 80 {
		t.Fatalf("watched_threshold = %v, want 80", create.WatchedThreshold)
	}
	if create.MinResumeThreshold == nil || *create.MinResumeThreshold != 10 {
		t.Fatalf("min_resume_threshold = %v, want 10", create.MinResumeThreshold)
	}

	// Omitted on create means inherit; the decoder leaves both nil.
	var bare createLibraryRequest
	if err := json.Unmarshal([]byte(`{"paths":["/m"],"type":"movies","name":"M"}`), &bare); err != nil {
		t.Fatalf("decode bare create: %v", err)
	}
	if bare.WatchedThreshold != nil || bare.MinResumeThreshold != nil {
		t.Fatalf("omitted create overrides = %v/%v, want nil", bare.WatchedThreshold, bare.MinResumeThreshold)
	}
}

func TestV1LibraryUpdatePreservesThresholdTriState(t *testing.T) {
	// Absent leaves the stored override unchanged.
	var absent updateLibraryRequest
	if err := json.Unmarshal([]byte(`{"name":"M"}`), &absent); err != nil {
		t.Fatalf("decode absent: %v", err)
	}
	if absent.WatchedThreshold.Set() || absent.MinResumeThreshold.Set() {
		t.Fatal("absent members were treated as set")
	}

	// An explicit null clears the override.
	var cleared updateLibraryRequest
	if err := json.Unmarshal([]byte(`{"watched_threshold":null}`), &cleared); err != nil {
		t.Fatalf("decode null: %v", err)
	}
	if !cleared.WatchedThreshold.Set() || cleared.WatchedThreshold.Value() != nil {
		t.Fatalf("null watched_threshold = (set=%v value=%v), want set+nil", cleared.WatchedThreshold.Set(), cleared.WatchedThreshold.Value())
	}

	// A number sets the override.
	var set updateLibraryRequest
	if err := json.Unmarshal([]byte(`{"min_resume_threshold":25}`), &set); err != nil {
		t.Fatalf("decode value: %v", err)
	}
	if !set.MinResumeThreshold.Set() || set.MinResumeThreshold.Value() == nil || *set.MinResumeThreshold.Value() != 25 {
		t.Fatalf("value min_resume_threshold = (set=%v value=%v), want set+25", set.MinResumeThreshold.Set(), set.MinResumeThreshold.Value())
	}
}

func TestV1LibraryResponseCarriesThresholdOverrides(t *testing.T) {
	watched, resume := 80, 10
	view := toLibraryResponse(&models.MediaFolder{ID: 1, Name: "M", WatchedThreshold: &watched, MinResumeThreshold: &resume})
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("encode view: %v", err)
	}
	if !strings.Contains(string(raw), `"watched_threshold":80`) || !strings.Contains(string(raw), `"min_resume_threshold":10`) {
		t.Fatalf("v1 library response drops overrides: %s", raw)
	}

	// Inherited overrides serialize as null, not omitted.
	raw, err = json.Marshal(toLibraryResponse(&models.MediaFolder{ID: 1, Name: "M"}))
	if err != nil {
		t.Fatalf("encode inherit view: %v", err)
	}
	if !strings.Contains(string(raw), `"watched_threshold":null`) || !strings.Contains(string(raw), `"min_resume_threshold":null`) {
		t.Fatalf("v1 library response should carry null overrides: %s", raw)
	}
}

func TestValidateLibraryThresholds(t *testing.T) {
	valid := []struct {
		name    string
		watched *int
		resume  *int
	}{
		{"both inherit", nil, nil},
		{"lower bounds", intPtr(1), intPtr(1)},
		{"upper bounds", intPtr(100), intPtr(99)},
		{"one side set", intPtr(50), nil},
		{"other side set", nil, intPtr(50)},
	}
	for _, tc := range valid {
		if err := validateLibraryThresholds(tc.watched, tc.resume); err != nil {
			t.Errorf("%s: validateLibraryThresholds = %v, want nil", tc.name, err)
		}
	}

	invalid := []struct {
		name    string
		watched *int
		resume  *int
		field   string
	}{
		{"watched zero", intPtr(0), nil, "watched_threshold"},
		{"watched negative", intPtr(-1), nil, "watched_threshold"},
		{"watched over 100", intPtr(101), nil, "watched_threshold"},
		{"resume zero", nil, intPtr(0), "min_resume_threshold"},
		{"resume negative", nil, intPtr(-1), "min_resume_threshold"},
		{"resume over 99", nil, intPtr(100), "min_resume_threshold"},
	}
	for _, tc := range invalid {
		err := validateLibraryThresholds(tc.watched, tc.resume)
		if err == nil {
			t.Errorf("%s: validateLibraryThresholds = nil, want an error", tc.name)
			continue
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Field != tc.field {
			t.Errorf("%s: error = %#v, want field %q", tc.name, err, tc.field)
		}
	}
}

func TestOptionalIntFieldTriState(t *testing.T) {
	if got := optionalIntField(optionalNullableInt{}); got.Set {
		t.Fatalf("unset field = %+v, want unset", got)
	}
	cleared := optionalNullableInt{}
	cleared.SetValue(nil, true)
	if got := optionalIntField(cleared); !got.Set || got.Value != nil {
		t.Fatalf("cleared field = %+v, want set+nil", got)
	}
	value := optionalNullableInt{}
	value.SetValue(intPtr(42), true)
	if got := optionalIntField(value); !got.Set || got.Value == nil || *got.Value != 42 {
		t.Fatalf("set field = %+v, want set+42", got)
	}
}

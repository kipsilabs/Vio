package apiv2

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/api/handlers"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/danielgtaylor/huma/v2"
)

func TestAdminCollectionUpdateCommandPreservesNullableGroup(t *testing.T) {
	for _, tc := range []struct {
		body  string
		set   bool
		group string
	}{
		{`{"library_ids":["12"],"title":"Edit"}`, false, ""},
		{`{"library_ids":["12"],"group_id":null}`, true, ""},
		{`{"group_id":"group-1"}`, true, "group-1"},
	} {
		t.Run(tc.body, func(t *testing.T) {
			var dto AdminCollectionUpdate
			if err := json.Unmarshal([]byte(tc.body), &dto); err != nil {
				t.Fatal(err)
			}
			cmd, p := dto.command([]byte(tc.body))
			if p != nil {
				t.Fatal(p)
			}
			if cmd.GroupID.Set() != tc.set {
				t.Fatalf("group set=%v want %v", cmd.GroupID.Set(), tc.set)
			}
			if tc.group == "" {
				if cmd.GroupID.Value() != nil {
					t.Fatalf("unexpected group: %v", cmd.GroupID.Value())
				}
			} else if cmd.GroupID.Value() == nil || *cmd.GroupID.Value() != tc.group {
				t.Fatal("group value lost")
			}
			if dto.LibraryIDs != nil && (cmd.LibraryIDs == nil || !reflect.DeepEqual(*cmd.LibraryIDs, []int{12})) {
				t.Fatalf("library IDs not lowered: %v", cmd.LibraryIDs)
			}
		})
	}
	// The raw bytes carry only null-presence information, never artwork or extra definition members.
	cmd, p := (AdminCollectionUpdate{Title: new("Typed")}).command([]byte(`{"title":"Raw","poster_source_url":"https://example.invalid/image"}`))
	if p != nil || cmd.Title == nil || *cmd.Title != "Typed" || cmd.PosterSourceURL != nil {
		t.Fatalf("raw body overrode typed input: %+v %v", cmd, p)
	}
}

func TestAdminCollectionCommandsValidateLibraryIdentifiers(t *testing.T) {
	cmd, p := (AdminCollectionCreate{LibraryID: "9", LibraryIDs: []ID{"9", "10"}, Title: "Create"}).command()
	if p != nil || cmd.LibraryID != 9 || !reflect.DeepEqual(cmd.LibraryIDs, []int{9, 10}) {
		t.Fatalf("bad lowering: %+v %v", cmd, p)
	}
	for _, id := range []ID{"bad", "0", "-1", "999999999999999999999999999"} {
		if _, p := (AdminMDBListImport{LibraryIDs: []ID{id}, Title: "Import", URL: "https://example.invalid/list"}).command(); p == nil {
			t.Fatalf("accepted library %q", id)
		}
	}
	good := AdminTemplateApply{LibraryIDs: []ID{"9", "10"}, Featured: &AdminTemplateFeatured{Home: &AdminTemplateFeaturedHome{LibraryID: "9", TemplateID: "home"}, Libraries: map[ID]string{"10": "library"}}}
	apply, p := good.command()
	if p != nil {
		t.Fatal(p)
	}
	if apply.Featured == nil || apply.Featured.Home.LibraryID != 9 || apply.Featured.Libraries[10] != "library" {
		t.Fatalf("nested IDs not lowered: %+v", apply)
	}
	good.Featured.Home.LibraryID = "invalid"
	if _, p = good.command(); p == nil {
		t.Fatal("accepted malformed nested home library")
	}
	good.Featured.Home = nil
	good.Featured.Libraries = map[ID]string{"bad": "template"}
	if _, p = good.command(); p == nil {
		t.Fatal("accepted malformed map key")
	}
}

func TestAdminCollectionDTOSchemaRejectsOptionalNull(t *testing.T) {
	for _, tc := range []struct {
		dto   any
		body  string
		valid bool
	}{
		{AdminCollectionUpdate{}, `{}`, true},
		{AdminCollectionUpdate{}, `{"group_id":null}`, true},
		{AdminCollectionCreate{}, `{"title":"Collection","library_id":"1"}`, true},
		{AdminCollectionCreate{}, `{"title":"Collection","library_id":1}`, false},
		{AdminCollectionCreate{}, `{"library_id":"1"}`, false},
		{AdminTemplateApply{}, `{"library_ids":["1"]}`, true},
	} {
		t.Run(tc.body, func(t *testing.T) {
			r := huma.NewMapRegistry("#/components/schemas/", huma.DefaultSchemaNamer)
			schema := huma.SchemaFromType(r, reflect.TypeOf(tc.dto))
			var value any
			if err := json.Unmarshal([]byte(tc.body), &value); err != nil {
				t.Fatal(err)
			}
			result := &huma.ValidateResult{}
			huma.Validate(r, schema, huma.NewPathBuffer([]byte("body"), 0), huma.ModeWriteToServer, value, result)
			if (len(result.Errors) == 0) != tc.valid {
				t.Fatalf("valid=%v errors=%v", tc.valid, result.Errors)
			}
		})
	}
}

func TestAdminCollectionResultSanitizesDiagnosticsAndDates(t *testing.T) {
	stamp := time.Date(2026, 9, 5, 12, 34, 56, 123456789, time.FixedZone("offset", 3600))
	run := adminCollectionSyncRunOf(&models.LibraryCollectionSyncRun{ID: "run", CollectionID: "collection", Status: "failed", Message: "PRIVATE_DIAGNOSTIC", Warnings: json.RawMessage(`["PRIVATE_WARNING"]`), CreatedAt: stamp, StartedAt: &stamp, CompletedAt: new(time.Time{})})
	data, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"PRIVATE_", "warnings", "completed_at", "0001-"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("unexpected %s in %s", forbidden, data)
		}
	}
	if !strings.Contains(string(data), `"created_at":"2026-09-05T11:34:56.123Z"`) {
		t.Fatalf("timestamp not normalized: %s", data)
	}
	var source handlers.AdminCollectionTemplateResult
	if err = json.Unmarshal([]byte(`{"bundle_id":"bundle","failed":[{"template_id":"a","library_id":9,"reason":"Collection already exists"}],"featured_failed":[{"surface":"home","template_id":"a","reason":"Library has no featured slot"}]}`), &source); err != nil {
		t.Fatal(err)
	}
	data, err = json.Marshal(adminTemplateResultOf(source))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "PRIVATE_") || strings.Contains(string(data), `"library_id":"0"`) {
		t.Fatalf("unsafe template result: %s", data)
	}
	// The per-entry reason is the only explanation a caller gets for a skipped
	// or failed template, and v1 has always returned it.
	if !strings.Contains(string(data), `"reason":"operation_failed"`) || strings.Contains(string(data), `Collection already exists`) {
		t.Fatalf("template entry reason dropped: %s", data)
	}
	if !strings.Contains(string(data), `"library_id":"9"`) || !strings.Contains(string(data), `"created":[]`) {
		t.Fatalf("lost identifiers or empty arrays: %s", data)
	}
	view := adminCollectionOf(handlers.AdminCollection{ID: "collection", LibraryID: 9, LibraryIDs: []int{9}, CreatedAt: stamp.Format(time.RFC3339Nano), UpdatedAt: stamp.Format(time.RFC3339Nano), LastSyncStatus: "failed", LastSyncMessage: "PRIVATE_MESSAGE"})
	data, err = json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "PRIVATE_") || strings.Contains(string(data), "last_sync_at") || strings.Contains(string(data), "next_sync_at") {
		t.Fatalf("invalid optional timestamps/diagnostic: %s", data)
	}
}

func TestAdminCollectionUpdateCommandRejectsNullBeforeLowering(t *testing.T) {
	r := huma.NewMapRegistry("#/components/schemas/", huma.DefaultSchemaNamer)
	schema := huma.SchemaFromType(r, reflect.TypeFor[AdminCollectionUpdate]())
	for _, field := range []string{"title", "library_ids", "featured", "description"} {
		if schema.Properties[field].Nullable {
			t.Fatalf("%s declared nullable", field)
		}
		raw := []byte(`{"` + field + `":null}`)
		var dto AdminCollectionUpdate
		if err := json.Unmarshal(raw, &dto); err != nil {
			t.Fatal(err)
		}
		if _, p := dto.command(raw); p == nil {
			t.Fatalf("accepted explicit null for %s", field)
		}
	}
	if !schema.Properties["group_id"].Nullable {
		t.Fatal("group_id clearing not declared nullable")
	}
}

// TestAdminCollectionCommandVirtualPlaybackTriState pins the v2 boundary for
// the additive virtual_playback field: an omitted value must stay a nil
// pointer so the handler default (on) applies, and an explicit value must
// survive the adapter unchanged. The handler side maps nil to true in
// TestResolveVirtualPlayback (internal/api/handlers), so omitted resolving to
// on is pinned on both sides of the boundary.
func TestAdminCollectionCommandVirtualPlaybackTriState(t *testing.T) {
	type commandCase struct {
		name  string
		build func(t *testing.T, field string) (*bool, *Problem)
	}
	cases := []commandCase{
		{
			name: "mdblist_import",
			build: func(t *testing.T, field string) (*bool, *Problem) {
				t.Helper()
				var dto AdminMDBListImport
				body := `{"library_ids":["1"],"title":"Import","url":"https://mdblist.com/lists/user/list"` + field + `}`
				if err := json.Unmarshal([]byte(body), &dto); err != nil {
					t.Fatal(err)
				}
				cmd, p := dto.command()
				return cmd.VirtualPlayback, p
			},
		},
		{
			name: "tmdb_import",
			build: func(t *testing.T, field string) (*bool, *Problem) {
				t.Helper()
				var dto AdminTMDBImport
				body := `{"library_ids":["1"],"title":"Import","preset":"popular","media_type":"movie"` + field + `}`
				if err := json.Unmarshal([]byte(body), &dto); err != nil {
					t.Fatal(err)
				}
				cmd, p := dto.command()
				return cmd.VirtualPlayback, p
			},
		},
		{
			name: "template_apply",
			build: func(t *testing.T, field string) (*bool, *Problem) {
				t.Helper()
				var dto AdminTemplateApply
				body := `{"library_ids":["1"]` + field + `}`
				if err := json.Unmarshal([]byte(body), &dto); err != nil {
					t.Fatal(err)
				}
				cmd, p := dto.command()
				return cmd.VirtualPlayback, p
			},
		},
	}
	variants := []struct {
		name      string
		field     string
		wantNil   bool
		wantValue bool
	}{
		{name: "omitted", field: "", wantNil: true},
		{name: "explicit_false", field: `,"virtual_playback":false`, wantValue: false},
		{name: "explicit_true", field: `,"virtual_playback":true`, wantValue: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, variant := range variants {
				t.Run(variant.name, func(t *testing.T) {
					got, p := tc.build(t, variant.field)
					if p != nil {
						t.Fatalf("command rejected: %v", p)
					}
					if variant.wantNil {
						if got != nil {
							t.Fatalf("omitted virtual_playback = %v, want nil so the handler default applies", *got)
						}
						return
					}
					if got == nil || *got != variant.wantValue {
						t.Fatalf("virtual_playback = %v, want %v", got, variant.wantValue)
					}
				})
			}
		})
	}
}

// TestAdminCollectionRequestSchemaExposesOptionalVirtualPlayback locks the
// additive contract: virtual_playback is an optional, non-nullable boolean on
// each of the four collection-creation request bodies.
func TestAdminCollectionRequestSchemaExposesOptionalVirtualPlayback(t *testing.T) {
	r := huma.NewMapRegistry("#/components/schemas/", huma.DefaultSchemaNamer)
	for _, tc := range []struct {
		name string
		dto  any
	}{
		{"AdminMDBListImport", AdminMDBListImport{}},
		{"AdminTMDBImport", AdminTMDBImport{}},
		{"AdminTemplateApply", AdminTemplateApply{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			schema := huma.SchemaFromType(r, reflect.TypeOf(tc.dto))
			prop, ok := schema.Properties["virtual_playback"]
			if !ok {
				t.Fatal("virtual_playback missing from schema")
			}
			if prop.Type != "boolean" {
				t.Fatalf("virtual_playback type = %q, want boolean", prop.Type)
			}
			if prop.Nullable {
				t.Fatal("virtual_playback must not be nullable")
			}
			if slices.Contains(schema.Required, "virtual_playback") {
				t.Fatal("virtual_playback must be optional")
			}
		})
	}
}

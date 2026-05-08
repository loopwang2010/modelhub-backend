// Coverage-targeted tests for branches the smoke suite (registry_test.go)
// doesn't naturally exercise. Per plans/CODE-REVIEW.md F6: catalog was at
// 77.5%, just under the 80% spec target.
//
// Coverage focus:
//   - registry.go: MustRegister (happy + panic), RegisterAll (happy +
//     first-error), package-level wrappers (MustRegister, RegisterAll,
//     ListEnabled).
//   - manifest.go: Validate (every error branch), PublicJSON.

package catalog

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/internal/adapter"
)

// ─────────────────────────────────────────────────────────────────────────
// Registry.MustRegister + Registry.RegisterAll
// ─────────────────────────────────────────────────────────────────────────

func TestRegistry_MustRegister_HappyPath(t *testing.T) {
	r := NewRegistry()
	defer func() {
		if rec := recover(); rec != nil {
			t.Errorf("MustRegister of valid manifest panicked: %v", rec)
		}
	}()
	r.MustRegister(makeManifest("must-ok", 0))
	if r.Len() != 1 {
		t.Errorf("Len = %d; want 1", r.Len())
	}
}

func TestRegistry_MustRegister_PanicsOnDuplicate(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(makeManifest("must-dup", 0))
	defer func() {
		rec := recover()
		if rec == nil {
			t.Fatal("MustRegister of duplicate did not panic")
		}
		if !strings.Contains(rec.(string), "MustRegister") {
			t.Errorf("panic message = %q; want it to mention MustRegister", rec)
		}
	}()
	r.MustRegister(makeManifest("must-dup", 1)) // duplicate key → panic
}

func TestRegistry_RegisterAll_HappyPath(t *testing.T) {
	r := NewRegistry()
	manifests := []ModelManifest{
		makeManifest("ra-1", 0),
		makeManifest("ra-2", 1),
		makeManifest("ra-3", 2),
	}
	if err := r.RegisterAll(manifests); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}
	if r.Len() != 3 {
		t.Errorf("Len = %d; want 3", r.Len())
	}
}

// RegisterAll stops at the first error and surfaces it. The dup row is
// the second manifest — so the first should land but the third never
// gets a chance.
func TestRegistry_RegisterAll_StopsOnFirstError(t *testing.T) {
	r := NewRegistry()
	manifests := []ModelManifest{
		makeManifest("ra-good", 0),
		makeManifest("ra-good", 1), // duplicate key
		makeManifest("ra-never", 2),
	}
	err := r.RegisterAll(manifests)
	if err == nil {
		t.Fatal("expected error from duplicate key")
	}
	if r.Len() != 1 {
		t.Errorf("Len = %d; want 1 (first manifest only)", r.Len())
	}
	if _, err := r.Get("ra-never"); err == nil {
		t.Error("ra-never should not have been registered")
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Package-level wrappers around DefaultRegistry
// ─────────────────────────────────────────────────────────────────────────

func TestPackageLevel_MustRegisterAndRegisterAllAndListEnabled(t *testing.T) {
	DefaultRegistry.Reset()
	defer DefaultRegistry.Reset()

	// MustRegister + RegisterAll combined coverage.
	MustRegister(makeManifest("pkg-must", 0))
	if err := RegisterAll([]ModelManifest{
		makeManifest("pkg-all-1", 1),
		makeManifest("pkg-all-2", 2),
	}); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}
	if DefaultRegistry.Len() != 3 {
		t.Errorf("Len = %d; want 3", DefaultRegistry.Len())
	}

	// ListEnabled should return all 3 (default Enabled=true on Register).
	enabled := ListEnabled()
	if len(enabled) != 3 {
		t.Errorf("ListEnabled len = %d; want 3", len(enabled))
	}

	// Disable one and verify ListEnabled drops it.
	if err := DefaultRegistry.SetEnabled("pkg-all-1", false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	enabled = ListEnabled()
	if len(enabled) != 2 {
		t.Errorf("ListEnabled after disable len = %d; want 2", len(enabled))
	}
	for _, m := range enabled {
		if m.Key == "pkg-all-1" {
			t.Errorf("pkg-all-1 should be filtered out of ListEnabled")
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────
// manifest.go : Validate — every error branch
// ─────────────────────────────────────────────────────────────────────────

// good is the canonical valid manifest; per-branch tests mutate it minimally.
func goodManifest() ModelManifest {
	return ModelManifest{
		Key:           "valid",
		Name:          "Valid Model",
		Provider:      "p",
		UpstreamModel: "p-1",
		Modality:      adapter.ModalityImage,
		TaskKind:      adapter.TaskKindAsync,
		InputSchema:   json.RawMessage(`{"type":"object"}`),
	}
}

func TestManifestValidate_AllBranches(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(m *ModelManifest)
		wantErr  bool
		wantSubstr string
	}{
		{"valid", func(*ModelManifest) {}, false, ""},
		{"empty Key", func(m *ModelManifest) { m.Key = "" }, true, "Key"},
		{"empty Name", func(m *ModelManifest) { m.Name = "" }, true, "Name"},
		{"empty Provider", func(m *ModelManifest) { m.Provider = "" }, true, "Provider"},
		{"empty UpstreamModel", func(m *ModelManifest) { m.UpstreamModel = "" }, true, "UpstreamModel"},
		{"bogus Modality", func(m *ModelManifest) { m.Modality = "fictional" }, true, "Modality"},
		{"empty Modality", func(m *ModelManifest) { m.Modality = "" }, true, "Modality"},
		{"bogus TaskKind", func(m *ModelManifest) { m.TaskKind = "telepathic" }, true, "TaskKind"},
		{"empty TaskKind", func(m *ModelManifest) { m.TaskKind = "" }, true, "TaskKind"},
		{"empty InputSchema", func(m *ModelManifest) { m.InputSchema = nil }, true, "InputSchema"},
		{"invalid InputSchema JSON", func(m *ModelManifest) {
			m.InputSchema = json.RawMessage(`{not valid json`)
		}, true, "InputSchema must be valid JSON"},
		// Each modality enum value must round-trip.
		{"ModalityVideo OK", func(m *ModelManifest) { m.Modality = adapter.ModalityVideo }, false, ""},
		{"ModalityAudio OK", func(m *ModelManifest) { m.Modality = adapter.ModalityAudio }, false, ""},
		{"ModalityEdit OK", func(m *ModelManifest) { m.Modality = adapter.ModalityEdit }, false, ""},
		{"ModalityLLM OK", func(m *ModelManifest) { m.Modality = adapter.ModalityLLM }, false, ""},
		// Each task-kind enum value must round-trip.
		{"TaskKindSync OK", func(m *ModelManifest) { m.TaskKind = adapter.TaskKindSync }, false, ""},
		{"TaskKindStreaming OK", func(m *ModelManifest) { m.TaskKind = adapter.TaskKindStreaming }, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := goodManifest()
			tc.mutate(&m)
			err := m.Validate()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error containing %q", tc.wantSubstr)
				}
				if tc.wantSubstr != "" && !strings.Contains(err.Error(), tc.wantSubstr) {
					t.Errorf("error = %q; want substr %q", err.Error(), tc.wantSubstr)
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────
// manifest.go : PublicJSON
// ─────────────────────────────────────────────────────────────────────────

// PublicJSON must produce parseable JSON containing the public fields and
// must not surface fields tagged json:"-".
func TestManifestPublicJSON_RoundTrips(t *testing.T) {
	m := goodManifest()
	m.Key = "pub-1"
	m.Order = 7

	data, err := m.PublicJSON()
	if err != nil {
		t.Fatalf("PublicJSON: %v", err)
	}

	var roundTripped map[string]any
	if err := json.Unmarshal(data, &roundTripped); err != nil {
		t.Fatalf("unmarshal: %v\nraw: %s", err, data)
	}
	// Spot-check a couple of public fields are present.
	if got := roundTripped["key"]; got != "pub-1" {
		t.Errorf("public json key = %v; want pub-1", got)
	}
}

package modality_test

import (
	"testing"

	"github.com/QuantumNous/new-api/internal/adapter"
	"github.com/QuantumNous/new-api/internal/modality"
)

// TestAliasIdentity verifies that the modality package's type aliases are
// truly interchangeable with the adapter package — a direct assignment in
// either direction must compile and produce equal values without any
// explicit conversion call.
func TestAliasIdentity(t *testing.T) {
	t.Parallel()

	var fromAdapter adapter.Modality = adapter.ModalityImage
	var fromModality modality.Modality = fromAdapter // alias → no conversion
	if fromModality != modality.Image {
		t.Fatalf("alias mismatch: adapter.ModalityImage (%q) != modality.Image (%q)",
			fromAdapter, modality.Image)
	}

	var taskFromAdapter adapter.TaskKind = adapter.TaskKindAsync
	var taskFromModality modality.TaskKind = taskFromAdapter
	if taskFromModality != modality.Async {
		t.Fatalf("alias mismatch: adapter.TaskKindAsync (%q) != modality.Async (%q)",
			taskFromAdapter, modality.Async)
	}
}

func TestParseModality(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want modality.Modality
		ok   bool
	}{
		{"empty defaults to LLM", "", modality.LLM, true},
		{"image", "image", modality.Image, true},
		{"video uppercase trimmed", "  VIDEO  ", modality.Video, true},
		{"audio", "audio", modality.Audio, true},
		{"edit", "edit", modality.Edit, true},
		{"llm", "llm", modality.LLM, true},
		{"unknown rejected", "movie", "", false},
		{"whitespace only acts as empty", "   ", modality.LLM, true},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := modality.ParseModality(tc.in)
			if ok != tc.ok {
				t.Fatalf("ParseModality(%q): ok=%v, want %v", tc.in, ok, tc.ok)
			}
			if got != tc.want {
				t.Fatalf("ParseModality(%q): got %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseTaskKind(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want modality.TaskKind
		ok   bool
	}{
		{"empty defaults to streaming", "", modality.Streaming, true},
		{"sync", "sync", modality.Sync, true},
		{"async uppercase trimmed", " ASYNC ", modality.Async, true},
		{"streaming", "streaming", modality.Streaming, true},
		{"unknown rejected", "queued", "", false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := modality.ParseTaskKind(tc.in)
			if ok != tc.ok {
				t.Fatalf("ParseTaskKind(%q): ok=%v, want %v", tc.in, ok, tc.ok)
			}
			if got != tc.want {
				t.Fatalf("ParseTaskKind(%q): got %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestIsValidModality(t *testing.T) {
	t.Parallel()

	for _, m := range []modality.Modality{
		modality.Image, modality.Video, modality.Audio, modality.Edit, modality.LLM,
	} {
		if !modality.IsValidModality(m) {
			t.Errorf("IsValidModality(%q) = false, want true", m)
		}
	}
	for _, m := range []modality.Modality{"", "movie", "text", "image_url"} {
		if modality.IsValidModality(m) {
			t.Errorf("IsValidModality(%q) = true, want false", m)
		}
	}
}

func TestIsValidTaskKind(t *testing.T) {
	t.Parallel()

	for _, k := range []modality.TaskKind{modality.Sync, modality.Async, modality.Streaming} {
		if !modality.IsValidTaskKind(k) {
			t.Errorf("IsValidTaskKind(%q) = false, want true", k)
		}
	}
	for _, k := range []modality.TaskKind{"", "queued", "batch"} {
		if modality.IsValidTaskKind(k) {
			t.Errorf("IsValidTaskKind(%q) = true, want false", k)
		}
	}
}

func TestNormalizeModality(t *testing.T) {
	t.Parallel()

	if got := modality.NormalizeModality(modality.Video); got != modality.Video {
		t.Errorf("NormalizeModality(Video) = %q, want Video", got)
	}
	if got := modality.NormalizeModality(""); got != modality.DefaultModality {
		t.Errorf("NormalizeModality(\"\") = %q, want DefaultModality (%q)", got, modality.DefaultModality)
	}
	if got := modality.NormalizeModality("garbage"); got != modality.DefaultModality {
		t.Errorf("NormalizeModality(garbage) = %q, want DefaultModality", got)
	}
}

func TestNormalizeTaskKind(t *testing.T) {
	t.Parallel()

	if got := modality.NormalizeTaskKind(modality.Async); got != modality.Async {
		t.Errorf("NormalizeTaskKind(Async) = %q, want Async", got)
	}
	if got := modality.NormalizeTaskKind(""); got != modality.DefaultTaskKind {
		t.Errorf("NormalizeTaskKind(\"\") = %q, want DefaultTaskKind (%q)", got, modality.DefaultTaskKind)
	}
	if got := modality.NormalizeTaskKind("garbage"); got != modality.DefaultTaskKind {
		t.Errorf("NormalizeTaskKind(garbage) = %q, want DefaultTaskKind", got)
	}
}

// TestDefaultsMatchSpec pins the backwards-compat defaults documented in
// BLUEPRINT.md §S3 — if a future refactor changes them, this test fails
// and forces the change to be intentional.
func TestDefaultsMatchSpec(t *testing.T) {
	t.Parallel()

	if modality.DefaultModality != modality.LLM {
		t.Errorf("DefaultModality = %q, want LLM (BLUEPRINT.md §S3 backwards-compat)", modality.DefaultModality)
	}
	if modality.DefaultTaskKind != modality.Streaming {
		t.Errorf("DefaultTaskKind = %q, want Streaming (BLUEPRINT.md §S3 backwards-compat)", modality.DefaultTaskKind)
	}
}

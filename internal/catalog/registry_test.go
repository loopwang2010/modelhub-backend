package catalog

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/internal/adapter"
)

func makeManifest(key adapter.ModelKey, order int) ModelManifest {
	return ModelManifest{
		Key:           key,
		Name:          string(key),
		Modality:      adapter.ModalityImage,
		TaskKind:      adapter.TaskKindAsync,
		Provider:      adapter.ProviderKey("test"),
		UpstreamModel: string(key) + "-upstream",
		InputSchema:   json.RawMessage(`{"type":"object"}`),
		PriceFormula:  "$0.01 per call",
		Order:         order,
	}
}

func TestRegistry_RegisterAndGet(t *testing.T) {
	r := NewRegistry()
	m := makeManifest("test-model-1", 0)

	if err := r.Register(m); err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	got, err := r.Get("test-model-1")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if got.Name != m.Name {
		t.Errorf("Get returned wrong manifest: %v", got)
	}
	if !got.Enabled {
		t.Errorf("Newly registered manifest should be enabled by default")
	}
}

func TestRegistry_RegisterDuplicateRejected(t *testing.T) {
	r := NewRegistry()
	m := makeManifest("dup-key", 0)
	if err := r.Register(m); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	err := r.Register(m)
	if !errors.Is(err, ErrManifestExists) {
		t.Errorf("duplicate Register: got %v; want ErrManifestExists", err)
	}
}

func TestRegistry_GetUnknownReturnsNotFound(t *testing.T) {
	r := NewRegistry()
	_, err := r.Get("does-not-exist")
	if !errors.Is(err, ErrManifestNotFound) {
		t.Errorf("Get unknown: got %v; want ErrManifestNotFound", err)
	}
}

func TestRegistry_ListSortedByOrderThenKey(t *testing.T) {
	r := NewRegistry()
	for _, m := range []ModelManifest{
		makeManifest("z-model", 0),
		makeManifest("a-model", 0),
		makeManifest("m-model", -1),
	} {
		if err := r.Register(m); err != nil {
			t.Fatalf("Register %s: %v", m.Key, err)
		}
	}
	list := r.List()
	if len(list) != 3 {
		t.Fatalf("List length = %d; want 3", len(list))
	}
	// m-model has lowest order → first; then alphabetical for the tied ones.
	want := []adapter.ModelKey{"m-model", "a-model", "z-model"}
	for i, m := range list {
		if m.Key != want[i] {
			t.Errorf("List[%d].Key = %q; want %q", i, m.Key, want[i])
		}
	}
}

func TestRegistry_SetEnabledFiltersListEnabled(t *testing.T) {
	r := NewRegistry()
	for _, k := range []adapter.ModelKey{"on-1", "off-1", "on-2"} {
		if err := r.Register(makeManifest(k, 0)); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}
	if err := r.SetEnabled("off-1", false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	enabled := r.ListEnabled()
	if len(enabled) != 2 {
		t.Errorf("ListEnabled len = %d; want 2", len(enabled))
	}
	for _, m := range enabled {
		if m.Key == "off-1" {
			t.Errorf("disabled manifest leaked into ListEnabled")
		}
		if !m.Enabled {
			t.Errorf("ListEnabled returned disabled entry %s", m.Key)
		}
	}
	all := r.List()
	if len(all) != 3 {
		t.Errorf("List len = %d; want 3 (includes disabled)", len(all))
	}
}

func TestRegistry_SetEnabledUnknownReturnsNotFound(t *testing.T) {
	r := NewRegistry()
	if err := r.SetEnabled("ghost", false); !errors.Is(err, ErrManifestNotFound) {
		t.Errorf("SetEnabled unknown: got %v; want ErrManifestNotFound", err)
	}
}

func TestRegistry_RegisterRejectsInvalid(t *testing.T) {
	r := NewRegistry()
	bad := makeManifest("invalid", 0)
	bad.UpstreamModel = "" // Validate requires this
	err := r.Register(bad)
	if !errors.Is(err, ErrManifestInvalid) {
		t.Errorf("Register invalid: got %v; want ErrManifestInvalid", err)
	}
}

func TestRegistry_ConcurrentRegisterAndList(t *testing.T) {
	r := NewRegistry()
	var wg sync.WaitGroup
	const writers = 8
	const reads = 100

	// Pre-register some entries so List always has work to do.
	for i := 0; i < 5; i++ {
		_ = r.Register(makeManifest(adapter.ModelKey("seed-"+string(rune('a'+i))), 0))
	}

	for w := 0; w < writers; w++ {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				_ = r.Register(makeManifest(adapter.ModelKey("w-"+string(rune('a'+w))+string(rune('0'+i))), 0))
			}
		}()
	}
	for i := 0; i < reads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = r.List()
			_ = r.Len()
		}()
	}
	wg.Wait()
}

func TestPackageLevelHelpers_UseDefaultRegistry(t *testing.T) {
	// Reset so other tests don't pollute.
	DefaultRegistry.Reset()
	defer DefaultRegistry.Reset()

	if err := Register(makeManifest("pkg-level", 0)); err != nil {
		t.Fatalf("Register: %v", err)
	}
	got, err := Get("pkg-level")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Key != "pkg-level" {
		t.Errorf("Get returned wrong key: %v", got)
	}
	if len(List()) != 1 {
		t.Errorf("List len = %d; want 1", len(List()))
	}
}

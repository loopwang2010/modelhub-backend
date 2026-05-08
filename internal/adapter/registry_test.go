package adapter

import (
	"errors"
	"sync"
	"testing"
)

// keyedMock wraps MockSyncAdapter to override Key() so registry tests can
// register many adapters under distinct keys without coupling to the
// fixed "mock-sync"/"mock-async" keys returned by the real mocks.
type keyedMock struct {
	*MockSyncAdapter
	k ProviderKey
}

func (k *keyedMock) Key() ProviderKey { return k.k }

func newKeyedMock(key ProviderKey) ProviderAdapter {
	return &keyedMock{MockSyncAdapter: NewMockSyncAdapter(), k: key}
}

func TestRegistry_RegisterAndGet(t *testing.T) {
	r := NewRegistry()
	a := newKeyedMock("p1")
	if err := r.Register(a); err != nil {
		t.Fatalf("Register: %v", err)
	}
	got, ok := r.Get("p1")
	if !ok {
		t.Fatal("Get returned false for registered key")
	}
	if got != a {
		t.Errorf("Get returned wrong adapter; want %p got %p", a, got)
	}
	if r.Len() != 1 {
		t.Errorf("Len = %d, want 1", r.Len())
	}
}

func TestRegistry_GetMiss(t *testing.T) {
	r := NewRegistry()
	if _, ok := r.Get("nope"); ok {
		t.Fatal("Get on empty registry returned ok=true")
	}
}

func TestRegistry_RegisterDuplicate(t *testing.T) {
	r := NewRegistry()
	a := newKeyedMock("dup")
	if err := r.Register(a); err != nil {
		t.Fatal(err)
	}
	err := r.Register(newKeyedMock("dup"))
	if err == nil {
		t.Fatal("expected ErrAlreadyRegistered")
	}
	if !errors.Is(err, ErrAlreadyRegistered) {
		t.Errorf("err = %v, want ErrAlreadyRegistered", err)
	}
}

func TestRegistry_RegisterNilOrEmptyKey(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(nil); err == nil {
		t.Error("Register(nil) returned nil error")
	}
	if err := r.Register(newKeyedMock("")); err == nil {
		t.Error("Register(adapter with empty key) returned nil error")
	}
}

func TestRegistry_Replace(t *testing.T) {
	r := NewRegistry()
	a1 := newKeyedMock("k")
	a2 := newKeyedMock("k")
	prev, err := r.Replace(a1)
	if err != nil {
		t.Fatal(err)
	}
	if prev != nil {
		t.Errorf("first Replace prev = %v, want nil", prev)
	}
	prev, err = r.Replace(a2)
	if err != nil {
		t.Fatal(err)
	}
	if prev != a1 {
		t.Errorf("second Replace prev = %v, want a1", prev)
	}
	got, _ := r.Get("k")
	if got != a2 {
		t.Errorf("Get after Replace returned wrong adapter")
	}
}

func TestRegistry_ReplaceRejectsNilOrEmpty(t *testing.T) {
	r := NewRegistry()
	if _, err := r.Replace(nil); err == nil {
		t.Error("Replace(nil) returned nil error")
	}
	if _, err := r.Replace(newKeyedMock("")); err == nil {
		t.Error("Replace(empty-key) returned nil error")
	}
}

func TestRegistry_ListIsSortedAndStable(t *testing.T) {
	r := NewRegistry()
	for _, k := range []ProviderKey{"c", "a", "b"} {
		if err := r.Register(newKeyedMock(k)); err != nil {
			t.Fatal(err)
		}
	}
	list := r.List()
	if len(list) != 3 {
		t.Fatalf("List len = %d", len(list))
	}
	want := []ProviderKey{"a", "b", "c"}
	for i, a := range list {
		if a.Key() != want[i] {
			t.Errorf("List[%d] = %q, want %q", i, a.Key(), want[i])
		}
	}
}

func TestRegistry_Keys(t *testing.T) {
	r := NewRegistry()
	r.Register(newKeyedMock("z"))
	r.Register(newKeyedMock("a"))
	r.Register(newKeyedMock("m"))
	keys := r.Keys()
	if len(keys) != 3 {
		t.Fatalf("Keys len = %d", len(keys))
	}
	want := []ProviderKey{"a", "m", "z"}
	for i, k := range keys {
		if k != want[i] {
			t.Errorf("Keys[%d] = %q, want %q", i, k, want[i])
		}
	}
}

func TestRegistry_Unregister(t *testing.T) {
	r := NewRegistry()
	r.Register(newKeyedMock("k"))
	if !r.Unregister("k") {
		t.Error("Unregister returned false for present key")
	}
	if _, ok := r.Get("k"); ok {
		t.Error("Get returned true after Unregister")
	}
	if r.Unregister("k") {
		t.Error("second Unregister returned true")
	}
}

func TestRegistry_MustGet(t *testing.T) {
	r := NewRegistry()
	r.Register(newKeyedMock("ok"))
	got := r.MustGet("ok")
	if got.Key() != "ok" {
		t.Error("MustGet returned wrong adapter")
	}
	defer func() {
		if recover() == nil {
			t.Error("MustGet on missing key did not panic")
		}
	}()
	r.MustGet("missing")
}

// TestRegistry_ConcurrentRegisterAndGet exercises the lock under load.
// Without -race we can still detect map-write panics or stale reads.
func TestRegistry_ConcurrentRegisterAndGet(t *testing.T) {
	r := NewRegistry()
	const writers = 8
	const readers = 8
	const ops = 200

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < ops; i++ {
				key := ProviderKey(rune('a'+w))
				_, _ = r.Replace(newKeyedMock(key))
			}
		}()
	}
	for rdr := 0; rdr < readers; rdr++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < ops; i++ {
				_ = r.List()
				_ = r.Keys()
				_, _ = r.Get("a")
				_ = r.Len()
			}
		}()
	}
	wg.Wait()
	if r.Len() == 0 {
		t.Fatal("expected at least one registered adapter")
	}
}

func TestRegistry_DefaultPackageHelpers(t *testing.T) {
	// Use Replace on default registry to avoid clobbering anything; clean up after.
	a := newKeyedMock("default-helper-test-key")
	prev, err := defaultRegistry.Replace(a)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if prev != nil {
			defaultRegistry.Replace(prev)
		} else {
			defaultRegistry.Unregister("default-helper-test-key")
		}
	})
	got, ok := Get("default-helper-test-key")
	if !ok || got != a {
		t.Error("default Get failed")
	}
	found := false
	for _, x := range List() {
		if x.Key() == "default-helper-test-key" {
			found = true
			break
		}
	}
	if !found {
		t.Error("default List did not include adapter")
	}
}

func TestRegistry_DefaultRegisterAndConvenience(t *testing.T) {
	a := newKeyedMock("convenience-key-xyz")
	defer defaultRegistry.Unregister("convenience-key-xyz")
	if err := Register(a); err != nil {
		t.Fatalf("default Register: %v", err)
	}
	if _, ok := Get("convenience-key-xyz"); !ok {
		t.Error("default Get failed")
	}
	// Re-register should fail.
	if err := Register(a); err == nil {
		t.Error("default Register dup did not error")
	}
}

func TestRegistry_MustRegisterPanicsOnDup(t *testing.T) {
	a := newKeyedMock("must-register-test")
	defer defaultRegistry.Unregister("must-register-test")
	MustRegister(a)
	defer func() {
		if recover() == nil {
			t.Error("expected MustRegister panic on duplicate")
		}
	}()
	MustRegister(a)
}

func TestSentinelErrors(t *testing.T) {
	if ErrAlreadyRegistered.Error() == "" {
		t.Error("ErrAlreadyRegistered empty")
	}
	if ErrNotRegistered.Error() == "" {
		t.Error("ErrNotRegistered empty")
	}
}

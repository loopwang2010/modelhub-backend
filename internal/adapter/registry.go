// Registry — the central ProviderKey → ProviderAdapter table.
//
// Owned by S2.5. Per blueprint S2.5 anti-pattern guard:
//
//	"internal/adapter/registry.go is OWNED by this PR. S7-S9 reviewers
//	 BLOCK any change to this file outside the package owners."
//
// S7-S9 (and any future provider integration) MUST register from their
// own subpackage's init() — never edit this file. New providers ship as
// new files, not edits here.
//
// Concurrency:
//   - Register is safe for use during package init (sequential, no contention)
//     and at runtime (e.g. dynamically loaded plugins).
//   - Get / List are concurrency-safe; List returns a fresh slice each call
//     so callers can hold/iterate without lock pressure.
//   - The package-level `defaultRegistry` exists for the common case of "one
//     registry per process". Tests construct private NewRegistry() instances
//     to avoid cross-test contamination.

package adapter

import (
	"fmt"
	"sort"
	"sync"
)

// Registry maps ProviderKey to ProviderAdapter. The zero value is NOT
// usable — construct via NewRegistry().
type Registry struct {
	mu       sync.RWMutex
	adapters map[ProviderKey]ProviderAdapter
}

// NewRegistry returns an empty Registry safe for concurrent use.
func NewRegistry() *Registry {
	return &Registry{
		adapters: make(map[ProviderKey]ProviderAdapter),
	}
}

// Register inserts adapter under its Key(). Returns ErrAlreadyRegistered
// when the key is already present (overwriting silently is a footgun —
// callers must explicitly Replace if that's the intent).
func (r *Registry) Register(adapter ProviderAdapter) error {
	if adapter == nil {
		return fmt.Errorf("registry: cannot register nil adapter")
	}
	key := adapter.Key()
	if key == "" {
		return fmt.Errorf("registry: adapter %T has empty Key()", adapter)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.adapters[key]; exists {
		return fmt.Errorf("%w: %q", ErrAlreadyRegistered, key)
	}
	r.adapters[key] = adapter
	return nil
}

// Replace inserts adapter, overwriting any prior registration under the
// same key. Returns the previous adapter (or nil) so callers can undo
// in tests via defer.
func (r *Registry) Replace(adapter ProviderAdapter) (ProviderAdapter, error) {
	if adapter == nil {
		return nil, fmt.Errorf("registry: cannot register nil adapter")
	}
	key := adapter.Key()
	if key == "" {
		return nil, fmt.Errorf("registry: adapter %T has empty Key()", adapter)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	prev := r.adapters[key]
	r.adapters[key] = adapter
	return prev, nil
}

// Get returns the adapter registered for key, or false.
func (r *Registry) Get(key ProviderKey) (ProviderAdapter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.adapters[key]
	return a, ok
}

// MustGet returns the adapter or panics. Useful in tests and bootstrap
// where missing-adapter is a programmer error, not runtime.
func (r *Registry) MustGet(key ProviderKey) ProviderAdapter {
	a, ok := r.Get(key)
	if !ok {
		panic(fmt.Sprintf("registry: no adapter registered for key %q", key))
	}
	return a
}

// List returns all registered adapters in deterministic order (by Key,
// ascending). The returned slice is a fresh copy — safe for callers to
// retain and iterate without holding the registry lock.
//
// Used by /v1/models catalog enumeration (S4) and admin tooling.
func (r *Registry) List() []ProviderAdapter {
	r.mu.RLock()
	keys := make([]ProviderKey, 0, len(r.adapters))
	for k := range r.adapters {
		keys = append(keys, k)
	}
	r.mu.RUnlock()
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	out := make([]ProviderAdapter, 0, len(keys))
	r.mu.RLock()
	for _, k := range keys {
		if a, ok := r.adapters[k]; ok {
			out = append(out, a)
		}
	}
	r.mu.RUnlock()
	return out
}

// Keys returns just the registered keys, sorted ascending. Cheaper than
// List when callers only need to enumerate identifiers.
func (r *Registry) Keys() []ProviderKey {
	r.mu.RLock()
	keys := make([]ProviderKey, 0, len(r.adapters))
	for k := range r.adapters {
		keys = append(keys, k)
	}
	r.mu.RUnlock()
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

// Len returns the number of registered adapters.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.adapters)
}

// Unregister removes the adapter for key. Returns true when the key was
// present. Provided for tests; production code does not unregister.
func (r *Registry) Unregister(key ProviderKey) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.adapters[key]; !ok {
		return false
	}
	delete(r.adapters, key)
	return true
}

// ─────────────────────────────────────────────────────────────────────────
// Package-level default registry
// ─────────────────────────────────────────────────────────────────────────
//
// One process, one default registry. Adapter packages call Register() from
// init() to populate it. main.go's bootstrap injects DEV_MODE mocks into the
// same default registry when the env flag is set.

var defaultRegistry = NewRegistry()

// DefaultRegistry returns the process-wide registry.
func DefaultRegistry() *Registry { return defaultRegistry }

// Register installs adapter into the default registry.
// Convenience wrapper for adapter init() functions.
func Register(adapter ProviderAdapter) error {
	return defaultRegistry.Register(adapter)
}

// MustRegister wraps Register and panics on error. Adapters call this from
// init() because a registration failure is a programmer error (duplicate
// key in a single binary).
func MustRegister(adapter ProviderAdapter) {
	if err := defaultRegistry.Register(adapter); err != nil {
		panic(fmt.Sprintf("MustRegister: %v", err))
	}
}

// Get is a default-registry convenience wrapper.
func Get(key ProviderKey) (ProviderAdapter, bool) { return defaultRegistry.Get(key) }

// List is a default-registry convenience wrapper.
func List() []ProviderAdapter { return defaultRegistry.List() }

// ─────────────────────────────────────────────────────────────────────────
// Sentinel errors
// ─────────────────────────────────────────────────────────────────────────

// ErrAlreadyRegistered is returned by Register when the key is taken.
// Wraps a clear sentinel so callers can branch via errors.Is.
var ErrAlreadyRegistered = newSentinel("registry: adapter already registered for key")

// ErrNotRegistered is returned when callers expect Get to succeed (e.g.
// Submit-time route lookup) and it doesn't.
var ErrNotRegistered = newSentinel("registry: no adapter registered for key")

type sentinelErr struct{ msg string }

func (e *sentinelErr) Error() string { return e.msg }

func newSentinel(msg string) error { return &sentinelErr{msg: msg} }

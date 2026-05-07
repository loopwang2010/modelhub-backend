// Catalog registry — holds the live ModelManifest set the API server serves
// at /v1/models. Populated by adapter package init()s during startup; can
// be admin-toggled at runtime via SetEnabled.
//
// In-memory only for MVP. Future enhancement: persist enabled-state to
// the wallet/admin DB so admin toggles survive restart. Manifests are
// always rebuilt from adapter source code, which is the canonical truth
// per ADR-007 (we don't trust DB-stored manifests as authoritative).
package catalog

import (
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/QuantumNous/new-api/internal/adapter"
)

// Registry holds the live set of ModelManifest entries.
type Registry struct {
	mu        sync.RWMutex
	manifests map[adapter.ModelKey]ModelManifest
	disabled  map[adapter.ModelKey]bool
}

// NewRegistry constructs an empty catalog registry.
func NewRegistry() *Registry {
	return &Registry{
		manifests: make(map[adapter.ModelKey]ModelManifest),
		disabled:  make(map[adapter.ModelKey]bool),
	}
}

// DefaultRegistry is the process-wide catalog. Adapter package init()s
// register their SeedManifests() here. /v1/models reads from here.
var DefaultRegistry = NewRegistry()

// Sentinel errors.
var (
	ErrManifestExists      = errors.New("catalog: manifest already registered for this key")
	ErrManifestNotFound    = errors.New("catalog: manifest not found")
	ErrManifestInvalid     = errors.New("catalog: manifest fails validation")
	ErrConflictingProvider = errors.New("catalog: manifest provider key conflicts with existing manifest's provider")
)

// Register adds a manifest to the registry. Returns ErrManifestExists if
// the model key is already taken. Validates the manifest before insert.
//
// Adapters call this from their init() AFTER registering with adapter.Registry.
func (r *Registry) Register(m ModelManifest) error {
	if err := m.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrManifestInvalid, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.manifests[m.Key]; exists {
		return fmt.Errorf("%w: %q", ErrManifestExists, m.Key)
	}
	// Default Enabled to true on register; admin toggles via SetEnabled.
	m.Enabled = true
	r.manifests[m.Key] = m
	return nil
}

// MustRegister panics on error. Convenience for adapter init()s where a
// duplicate-key panic correctly fails startup loudly.
func (r *Registry) MustRegister(m ModelManifest) {
	if err := r.Register(m); err != nil {
		panic("catalog: MustRegister: " + err.Error())
	}
}

// RegisterAll is a convenience for adapter SeedManifests() callers.
// Stops at the first error.
func (r *Registry) RegisterAll(manifests []ModelManifest) error {
	for _, m := range manifests {
		if err := r.Register(m); err != nil {
			return err
		}
	}
	return nil
}

// Get returns the manifest for a model key. Returns ErrManifestNotFound
// for unknown keys. Returns the manifest with Enabled set to its current
// admin state (true unless explicitly disabled).
func (r *Registry) Get(key adapter.ModelKey) (ModelManifest, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.manifests[key]
	if !ok {
		return ModelManifest{}, ErrManifestNotFound
	}
	m.Enabled = !r.disabled[key]
	return m, nil
}

// List returns all manifests sorted by (Order ASC, Key ASC). Includes both
// enabled and disabled entries — caller filters via Enabled field.
func (r *Registry) List() []ModelManifest {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ModelManifest, 0, len(r.manifests))
	for _, m := range r.manifests {
		m.Enabled = !r.disabled[m.Key]
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Order != out[j].Order {
			return out[i].Order < out[j].Order
		}
		return string(out[i].Key) < string(out[j].Key)
	})
	return out
}

// ListEnabled returns only entries where Enabled is true.
// This is what /v1/models serves.
func (r *Registry) ListEnabled() []ModelManifest {
	all := r.List()
	out := make([]ModelManifest, 0, len(all))
	for _, m := range all {
		if m.Enabled {
			out = append(out, m)
		}
	}
	return out
}

// SetEnabled flips the enabled flag for a model. Admin-only path.
// Returns ErrManifestNotFound for unknown keys.
func (r *Registry) SetEnabled(key adapter.ModelKey, enabled bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.manifests[key]; !ok {
		return ErrManifestNotFound
	}
	if enabled {
		delete(r.disabled, key)
	} else {
		r.disabled[key] = true
	}
	return nil
}

// Reset clears all manifests. Test-only.
func (r *Registry) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.manifests = make(map[adapter.ModelKey]ModelManifest)
	r.disabled = make(map[adapter.ModelKey]bool)
}

// Len returns the number of registered manifests.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.manifests)
}

// Package-level convenience wrappers around DefaultRegistry. These are
// what adapter init()s call.

// Register adds m to the DefaultRegistry.
func Register(m ModelManifest) error { return DefaultRegistry.Register(m) }

// MustRegister adds m to the DefaultRegistry, panicking on error.
func MustRegister(m ModelManifest) { DefaultRegistry.MustRegister(m) }

// RegisterAll adds all manifests to the DefaultRegistry.
func RegisterAll(manifests []ModelManifest) error { return DefaultRegistry.RegisterAll(manifests) }

// List returns all manifests in the DefaultRegistry.
func List() []ModelManifest { return DefaultRegistry.List() }

// ListEnabled returns enabled manifests in the DefaultRegistry.
func ListEnabled() []ModelManifest { return DefaultRegistry.ListEnabled() }

// Get returns a manifest by key from the DefaultRegistry.
func Get(key adapter.ModelKey) (ModelManifest, error) { return DefaultRegistry.Get(key) }

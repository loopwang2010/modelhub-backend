// Package init() registers the BFL adapter with the default registry —
// IFF the env is configured AND DEV_MODE != "mock".
//
// Per blueprint S2.5 task ownership, S7-S9 register from their own
// subpackage init() and never edit registry.go.
//
// The dev-mode short-circuit means:
//   - In production (BFL_API_KEY set, DEV_MODE unset): real adapter is registered.
//   - In dev (DEV_MODE=mock): main.go's MaybeBootstrapDevMode swaps in the mock
//     under the "bfl" key via Replace(); we skip here so we don't race.
//   - In tests / CI without BFL_API_KEY: registration silently skipped, no
//     panic. Tests that need the adapter construct one via New() directly.
//
// Registration failures (duplicate key) are logged via the standard library
// without panicking — a misconfigured env should never crash a long-running
// API server.

package bfl

import (
	"errors"
	"log"
	"os"

	"github.com/QuantumNous/new-api/internal/adapter"
	"github.com/QuantumNous/new-api/internal/catalog"
)

func init() {
	// Catalog manifests register UNCONDITIONALLY — even when the adapter
	// itself can't (no API key in CI, dev-mode mock taking the slot). The
	// catalog tells /v1/models what we OFFER; the adapter decides whether
	// we can actually serve it. A model whose adapter isn't registered
	// returns 503 at submit-time — better UX than hiding it from /v1/models.
	for _, m := range SeedManifests() {
		if err := catalog.Register(m); err != nil {
			// Duplicate-key during tests is fine; log but don't panic.
			log.Printf("bfl: catalog register %s: %v", m.Key, err)
		}
	}

	if os.Getenv(adapter.DevModeEnvVar) == adapter.DevModeValue {
		// dev-mode mocks take precedence; main.go's bootstrap will
		// register them under the canonical keys via Replace().
		return
	}
	a, err := NewFromEnv()
	if err != nil {
		// ErrNotConfigured is the expected case in test/CI runs without a
		// real API key. Swallow it to keep `go test ./...` quiet.
		if errors.Is(err, adapter.ErrNotConfigured) {
			return
		}
		log.Printf("bfl: adapter init error (skipping registration): %v", err)
		return
	}
	if err := adapter.Register(a); err != nil {
		log.Printf("bfl: register error: %v", err)
	}
}

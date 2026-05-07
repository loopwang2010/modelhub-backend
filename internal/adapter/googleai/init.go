// Package googleai — auto-registration in the default adapter registry.
//
// init() registers a GoogleVertexAIAdapter only when both:
//   - GOOGLE_APPLICATION_CREDENTIALS is set, AND
//   - DEV_MODE != "mock" (mocks own all provider keys in dev mode)
//
// Registration failures are logged (via fmt to stderr) but DO NOT panic;
// adapter packages must be import-safe even when their upstream is offline.
//
// The lookup is intentionally process-env-only (no flags, no config files)
// so the adapter's "is this configured?" question has a single source of
// truth that an operator can grep for.

package googleai

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"

	"github.com/QuantumNous/new-api/internal/adapter"
	"github.com/QuantumNous/new-api/internal/catalog"
)

// getenvLookup is the env-var lookup function used by loadConfig and init.
// Pulled out as a var so tests can override.
var getenvLookup = os.Getenv

// registerFunc is the registry write function. Pulled out as a var so
// tests can swap in a private registry.
var registerFunc = adapter.Register

func init() {
	// Catalog manifests register UNCONDITIONALLY (see bfl/init.go for rationale).
	for _, m := range SeedManifests() {
		if err := catalog.Register(m); err != nil {
			log.Printf("googleai: catalog register %s: %v", m.Key, err)
		}
	}

	if !shouldRegister(getenvLookup) {
		return
	}
	a, err := NewGoogleVertexAIAdapter(context.Background())
	if err != nil {
		// ErrNotConfigured is the expected outcome on a host without
		// Google credentials — log at info level instead of warning.
		// Other errors (e.g., blocked location) are visible to the
		// operator at startup.
		if errors.Is(err, adapter.ErrNotConfigured) {
			return
		}
		fmt.Fprintf(os.Stderr, "googleai: skipping registration: %v\n", err)
		return
	}
	if err := registerFunc(a); err != nil {
		fmt.Fprintf(os.Stderr, "googleai: registration failed: %v\n", err)
	}
}

// shouldRegister reports whether init should attempt to register the
// adapter, given the supplied env lookup. Pulled out for unit tests.
func shouldRegister(envLookup func(string) string) bool {
	if envLookup(adapter.DevModeEnvVar) == adapter.DevModeValue {
		// In DEV_MODE=mock the mocks own provider keys; do not contend.
		return false
	}
	return envLookup(CredentialsEnvVar) != ""
}

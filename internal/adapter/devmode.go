// DEV_MODE=mock bootstrap (per blueprint §S2.5 task 8 + review H4).
//
// When the env var DEV_MODE=mock is set at process start, RegisterDevModeMocks
// installs the sync and async mock adapters into the supplied registry under
// their canonical keys. The router can then serve `mock-image` / `mock-async-image`
// generations end-to-end without any provider env vars set.
//
// This function is intentionally explicit (not auto-init via package init)
// so production builds never accidentally ship with mocks registered.
//
// The dev-mode flag also informs callers that they MAY register mock
// MANIFESTS pointing at these provider keys; that wiring belongs in S3
// (manifest seed data), not here. This package only offers the adapter side.

package adapter

import (
	"errors"
	"os"
)

// DevModeEnvVar is the env var consulted by IsDevMode.
const DevModeEnvVar = "DEV_MODE"

// DevModeValue is the recognized value that activates mocks.
const DevModeValue = "mock"

// IsDevMode reports whether DEV_MODE=mock is set in the current process env.
func IsDevMode() bool {
	return os.Getenv(DevModeEnvVar) == DevModeValue
}

// RegisterDevModeMocks installs MockSyncAdapter and MockAsyncAdapter into
// reg under their canonical keys ("mock-sync", "mock-async"). Idempotent:
// re-registration via Replace lets repeat calls (e.g. test suites) succeed.
//
// Returns an error if reg is nil — programmer error.
func RegisterDevModeMocks(reg *Registry) error {
	if reg == nil {
		return errors.New("RegisterDevModeMocks: nil registry")
	}
	if _, err := reg.Replace(NewMockSyncAdapter()); err != nil {
		return err
	}
	if _, err := reg.Replace(NewMockAsyncAdapter()); err != nil {
		return err
	}
	return nil
}

// MaybeBootstrapDevMode is a convenience for main.go: checks IsDevMode()
// and registers mocks into the default registry. No-op when DEV_MODE != "mock".
// Returns true when mocks were registered, false otherwise.
func MaybeBootstrapDevMode() (bool, error) {
	if !IsDevMode() {
		return false, nil
	}
	if err := RegisterDevModeMocks(defaultRegistry); err != nil {
		return false, err
	}
	return true, nil
}

// init.go — package init() that auto-registers the OpenAI image
// adapter into the default ProviderAdapter registry.
//
// Skip conditions (any one is sufficient):
//
//   - OPENAI_API_KEY env var unset → cannot construct, ErrNotConfigured
//   - DEV_MODE=mock          → mock adapters are registered instead;
//                              the prod adapter would shadow them
//
// Both are silent skips: the absence of OpenAI in production builds is
// a deployment configuration choice, not a programming error. The
// caller (main.go bootstrap) can inspect adapter.List() to discover
// which providers are actually live.
//
// The fetcher injected here is a NIL placeholder until S9.5 ships the
// real object-storage backend; we keep the seam open by deferring
// registration to a function the bootstrap layer calls explicitly with
// a real fetcher. init() does NOT register the adapter directly —
// instead it caches the manifests for catalog seeding and waits for
// `RegisterWithFetcher` to be called.
//
// This means: the catalog can list gpt-image-* models even before the
// asset-storage backend is wired (so /v1/models is stable), but Submit
// requests fail-fast with a clear "not yet wired" error.

package openai

import (
	"errors"
	"log"

	"github.com/QuantumNous/new-api/internal/adapter"
	"github.com/QuantumNous/new-api/internal/catalog"
)

// init registers manifests in the catalog UNCONDITIONALLY (see bfl/init.go
// for rationale). The actual adapter registration is still deferred to
// RegisterWithFetcher — but /v1/models can list gpt-image-* models from
// the moment the package imports, even before the upload fetcher is wired.
func init() {
	manifests, err := SeedManifests()
	if err != nil {
		log.Printf("openai: SeedManifests build failed: %v", err)
		return
	}
	for _, m := range manifests {
		if err := catalog.Register(m); err != nil {
			log.Printf("openai: catalog register %s: %v", m.Key, err)
		}
	}
}

// ErrAdapterNotInitialized is returned when Submit is invoked before
// RegisterWithFetcher has been called. The bootstrap layer (main.go)
// is responsible for calling RegisterWithFetcher exactly once.
var ErrAdapterNotInitialized = errors.New("openai: adapter not initialized — RegisterWithFetcher must be called from main.go bootstrap")

// shouldRegister reports whether init-time registration is appropriate
// for the current process. False when:
//   - running in DEV_MODE=mock (mocks take over)
//   - OPENAI_API_KEY missing  (not configured)
func shouldRegister() bool {
	if adapter.IsDevMode() {
		return false
	}
	if _, err := loadClientConfig(); err != nil {
		return false
	}
	return true
}

// RegisterWithFetcher constructs the adapter with the provided upload
// fetcher and registers it into the default registry. Designed to be
// called once from main.go after the S9.5 asset/object-storage package
// is constructed.
//
// Idempotent: re-calling Replace's the prior registration. Returns
// ErrNotConfigured if OPENAI_API_KEY is missing; nil if DEV_MODE=mock
// (which signals "intentionally skipped" — caller logs but does not
// fatal).
func RegisterWithFetcher(fetcher uploadFetcher) error {
	if !shouldRegister() {
		// Silent skip; caller can detect via adapter.Get(ProviderKeyOpenAI)
		// returning false.
		return nil
	}
	a, err := New(fetcher)
	if err != nil {
		return err
	}
	if _, err := adapter.DefaultRegistry().Replace(a); err != nil {
		return err
	}
	return nil
}

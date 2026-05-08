// Package googleai — service-account auth handling.
//
// Per S7-S8-API-RESEARCH.md §2 and the S8 anti-pattern guard:
//
//	"Service account JSON MUST NOT be inline env var (per S8 anti-pattern
//	 guard); file path only."
//
// We therefore intentionally support ONLY the GOOGLE_APPLICATION_CREDENTIALS
// env var, which is the standard ADC convention and points at a file path.
// Inline JSON via env (e.g. GOOGLE_SERVICE_ACCOUNT_JSON) is explicitly NOT
// supported. If a future agent reaches for that, code review BLOCKS.
//
// The helpers in this file are deliberately small so the auth surface is
// easy to audit and unit-test.

package googleai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/internal/adapter"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// CredentialsEnvVar is the ONLY env var consulted for Google credentials.
// Maps to a file path on disk holding service account JSON.
const CredentialsEnvVar = "GOOGLE_APPLICATION_CREDENTIALS"

// VertexAIScope is the OAuth scope required for Vertex AI predictLongRunning.
const VertexAIScope = "https://www.googleapis.com/auth/cloud-platform"

// credentialSource resolves an *oauth2.TokenSource and the project ID for
// the configured service account. It is split out as an interface so tests
// can substitute a fake without touching the filesystem.
type credentialSource interface {
	tokenSource(ctx context.Context) (oauth2.TokenSource, error)
	projectID() string
}

// fileCredentialSource loads credentials from the path referenced by
// GOOGLE_APPLICATION_CREDENTIALS. It memoizes the parsed credential once
// to avoid repeated file reads on the hot path.
type fileCredentialSource struct {
	path string

	mu    sync.Mutex
	creds *google.Credentials
}

func newFileCredentialSource(path string) *fileCredentialSource {
	return &fileCredentialSource{path: path}
}

func (f *fileCredentialSource) load(ctx context.Context) (*google.Credentials, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.creds != nil {
		return f.creds, nil
	}
	if f.path == "" {
		return nil, fmt.Errorf("googleai: %w: GOOGLE_APPLICATION_CREDENTIALS unset", adapter.ErrNotConfigured)
	}
	data, err := os.ReadFile(f.path) // #nosec G304 -- operator-controlled path is required
	if err != nil {
		return nil, fmt.Errorf("googleai: %w: cannot read credentials at %q: %v", adapter.ErrNotConfigured, f.path, err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("googleai: %w: credentials file %q is empty", adapter.ErrNotConfigured, f.path)
	}
	c, err := google.CredentialsFromJSON(ctx, data, VertexAIScope)
	if err != nil {
		return nil, fmt.Errorf("googleai: %w: invalid credentials JSON: %v", adapter.ErrNotConfigured, err)
	}
	if c.ProjectID == "" {
		return nil, fmt.Errorf("googleai: %w: credentials missing project_id", adapter.ErrNotConfigured)
	}
	f.creds = c
	return c, nil
}

func (f *fileCredentialSource) tokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	c, err := f.load(ctx)
	if err != nil {
		return nil, err
	}
	return c.TokenSource, nil
}

func (f *fileCredentialSource) projectID() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.creds == nil {
		return ""
	}
	return f.creds.ProjectID
}

// staticTokenSource is a credentialSource that returns a fixed bearer
// token. Test-only; used by adapter_test.go to drive a fake httptest server
// without needing real Google credentials.
type staticTokenSource struct {
	token   string
	project string
}

func (s *staticTokenSource) tokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	if s.token == "" {
		return nil, fmt.Errorf("googleai: %w: empty static token", adapter.ErrNotConfigured)
	}
	return oauth2.StaticTokenSource(&oauth2.Token{
		AccessToken: s.token,
		TokenType:   "Bearer",
		Expiry:      time.Now().Add(time.Hour),
	}), nil
}

func (s *staticTokenSource) projectID() string { return s.project }

// authedClient returns an *http.Client that injects the OAuth2 access token
// on every request. Wraps the supplied base transport so callers (tests)
// can swap in an httptest round tripper.
func authedClient(ctx context.Context, src credentialSource, base http.RoundTripper) (*http.Client, error) {
	if src == nil {
		return nil, errors.New("googleai: nil credential source")
	}
	ts, err := src.tokenSource(ctx)
	if err != nil {
		return nil, err
	}
	transport := base
	if transport == nil {
		transport = http.DefaultTransport
	}
	return &http.Client{
		Transport: &oauth2.Transport{
			Base:   transport,
			Source: ts,
		},
		Timeout: 30 * time.Second,
	}, nil
}

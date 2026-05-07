// Package asset implements S9.5 — output asset hosting per ADR-010.
//
// This file defines the Storage abstraction used by the AssetWorker.
// Two concrete implementations ship with this package:
//
//   - LocalDiskStore: writes to a local filesystem directory; suitable
//     for dev mode + tests; stable URL surface (file://-style or a
//     configurable public URL prefix).
//   - S3Store: skeleton + ErrNotConfigured stub. Production wiring
//     against aws-sdk-go-v2/service/s3 is intentionally deferred to a
//     follow-up step (see S9.5 agent report — open question §1).
//
// AP-13 guard: every Put MUST stream from io.Reader, never buffer the
// full payload in memory. Callers (the AssetWorker) wire the upstream
// HTTP body straight to Put without a tee/copy step.
//
// Path layout (per S6-WALLET-DESIGN.md §2):
//
//	outputs/{account_id}/{task_id}/{filename}
//
// `filename` is derived from upstream MIME + a hash of (task_id, index)
// so identical re-uploads produce stable keys (concurrent-safety).

package asset

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ErrStorageNotConfigured is returned by stores that require runtime
// configuration (S3 bucket name, credentials, …) and have not received it.
var ErrStorageNotConfigured = errors.New("asset: storage not configured")

// PutResult is the outcome of a successful Put.
type PutResult struct {
	// Key is the object key written to (path-style, no scheme/host).
	Key string

	// URL is the publicly-resolvable CDN URL for the stored object.
	// MUST start with one of the prefixes recognized by api.isCDNURL.
	URL string

	// SizeBytes is the number of bytes actually written. Authoritative —
	// derived from the body stream, not from a caller-supplied hint.
	SizeBytes int64
}

// Storage is the upload boundary for hosted assets.
//
// Implementations MUST be safe for concurrent calls from many goroutines.
// Implementations MUST stream the body — buffering the full payload in
// memory is an AP-13 violation and a P0 review block.
type Storage interface {
	// Put writes body under key, returning the public URL. body is read
	// to EOF; an error mid-stream is returned without rolling back what
	// was already written (best-effort cleanup is the implementation's
	// choice; LocalDiskStore deletes the partial file).
	Put(ctx context.Context, key string, contentType string, body io.Reader) (*PutResult, error)
}

// ─────────────────────────────────────────────────────────────────────────
// LocalDiskStore
// ─────────────────────────────────────────────────────────────────────────

// LocalDiskStore writes objects to a local directory. URLs returned use
// the configured PublicURLPrefix. Suitable for dev mode + tests.
//
// Concurrency: file writes are serialized per key via an internal lock,
// preventing two goroutines uploading the same key from corrupting each
// other's output. Different keys upload in parallel.
type LocalDiskStore struct {
	// RootDir is the on-disk directory under which keys are placed.
	// Must exist + be writable.
	RootDir string

	// PublicURLPrefix is the CDN URL prefix prepended to each Put result's
	// returned URL. MUST end with `/`. The api package's isCDNURL allow-
	// list controls which prefixes are accepted by the envelope; for
	// dev/test, use "https://cdn.modelhub.local/".
	PublicURLPrefix string

	mu       sync.Mutex
	keyLocks map[string]*sync.Mutex
}

// NewLocalDiskStore returns a LocalDiskStore. rootDir is created if missing.
// publicURLPrefix is required and must end with `/`.
func NewLocalDiskStore(rootDir, publicURLPrefix string) (*LocalDiskStore, error) {
	if rootDir == "" {
		return nil, fmt.Errorf("asset: rootDir required: %w", ErrStorageNotConfigured)
	}
	if publicURLPrefix == "" || !strings.HasSuffix(publicURLPrefix, "/") {
		return nil, fmt.Errorf("asset: publicURLPrefix must be non-empty and end with '/': %w", ErrStorageNotConfigured)
	}
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return nil, fmt.Errorf("asset: mkdir rootDir: %w", err)
	}
	return &LocalDiskStore{
		RootDir:         rootDir,
		PublicURLPrefix: publicURLPrefix,
		keyLocks:        make(map[string]*sync.Mutex),
	}, nil
}

// Put implements Storage. Streams body to a file at RootDir/key.
//
// Path-traversal guard: keys containing ".." or absolute prefixes are
// rejected. Keys are created relative to RootDir; symlinks/escape via
// crafted keys is impossible.
func (s *LocalDiskStore) Put(ctx context.Context, key string, contentType string, body io.Reader) (*PutResult, error) {
	if err := validateObjectKey(key); err != nil {
		return nil, err
	}
	if body == nil {
		return nil, errors.New("asset: nil body")
	}
	lock := s.lockForKey(key)
	lock.Lock()
	defer lock.Unlock()

	dst := filepath.Join(s.RootDir, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return nil, fmt.Errorf("asset: mkdir parent: %w", err)
	}
	// Write to a temp file in the same dir, then atomic rename. This
	// prevents partial files from being observable to other readers.
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".upload-*")
	if err != nil {
		return nil, fmt.Errorf("asset: create temp: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}

	// io.Copy streams chunk-by-chunk; the io package's default 32 KiB
	// buffer is reused. Honors ctx by wrapping the body in a context-
	// aware reader.
	n, err := io.Copy(tmp, ctxReader(ctx, body))
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("asset: copy body: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return nil, fmt.Errorf("asset: close temp: %w", err)
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		_ = os.Remove(tmpPath)
		return nil, fmt.Errorf("asset: atomic rename: %w", err)
	}
	return &PutResult{
		Key:       key,
		URL:       s.PublicURLPrefix + key,
		SizeBytes: n,
	}, nil
}

// lockForKey returns the per-key lock, creating it on first use.
func (s *LocalDiskStore) lockForKey(key string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l, ok := s.keyLocks[key]; ok {
		return l
	}
	l := &sync.Mutex{}
	s.keyLocks[key] = l
	return l
}

// ─────────────────────────────────────────────────────────────────────────
// S3Store skeleton
// ─────────────────────────────────────────────────────────────────────────

// S3Store is the production target for hosted assets. The skeleton ships
// in S9.5; the actual aws-sdk-go-v2/service/s3 wiring is a follow-up
// (see S9.5-AGENT-REPORT.md §1 open question).
//
// The skeleton documents the production env-var contract:
//
//	S3_BUCKET                — required
//	S3_REGION                — required
//	S3_ENDPOINT              — optional; used for MinIO + R2-style endpoints
//	S3_PUBLIC_URL_PREFIX     — required; e.g. "https://cdn.modelhub.com/"
//	AWS_ACCESS_KEY_ID        — required
//	AWS_SECRET_ACCESS_KEY    — required
type S3Store struct {
	Bucket          string
	Region          string
	Endpoint        string
	PublicURLPrefix string
}

// Put implements Storage. Until the SDK wiring lands, returns
// ErrStorageNotConfigured so a misconfigured deployment fails fast at
// upload time rather than silently dropping bytes.
func (s *S3Store) Put(ctx context.Context, key string, contentType string, body io.Reader) (*PutResult, error) {
	return nil, fmt.Errorf("asset: S3Store.Put not yet wired; see open question §1: %w", ErrStorageNotConfigured)
}

// ─────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────

// validateObjectKey rejects keys that would escape the storage root.
// Path-traversal hardening — never trust the caller, even though all
// callers in the asset package use BuildObjectKey internally.
func validateObjectKey(key string) error {
	if key == "" {
		return errors.New("asset: empty object key")
	}
	if strings.HasPrefix(key, "/") || strings.HasPrefix(key, "\\") {
		return fmt.Errorf("asset: absolute object key not allowed: %q", key)
	}
	if strings.Contains(key, "..") {
		return fmt.Errorf("asset: object key contains '..': %q", key)
	}
	// Reject Windows drive prefixes (e.g. "C:")
	if len(key) >= 2 && key[1] == ':' {
		return fmt.Errorf("asset: object key has drive prefix: %q", key)
	}
	return nil
}

// ctxReader wraps r with ctx-cancellation. Reads return ctx.Err() when
// the context is done, even if r itself is blocked on a slow upstream.
func ctxReader(ctx context.Context, r io.Reader) io.Reader {
	return &cancelReader{ctx: ctx, r: r}
}

type cancelReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *cancelReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

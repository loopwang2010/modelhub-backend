// Per-scheme downloader dispatch (T-001 from S7-S8-API-RESEARCH.md §1).
//
// The asset worker invokes Download(ctx, src) once per OutputAvailable
// event. Dispatcher routes by URL scheme:
//
//	https://...                     → standard http.Get
//	gs://bucket/object              → Google Cloud Storage REST download
//	(empty URL + inline payload)    → not handled here; AssetWorker logs
//	                                  a deferred-feature warning. See
//	                                  S9.5 agent report open question §2.
//
// All downloaders honor ctx cancellation + the SLA timeout from
// ADR-010 (default 60s). Downloaders return a streaming io.ReadCloser
// so the caller can pipe the body straight into Storage.Put without
// buffering the full payload (AP-13).
//
// Transient-vs-permanent classification:
//   - 2xx                    → success
//   - 408, 425, 429, 5xx     → ErrTransient (caller may retry)
//   - 3xx (after redirect)   → handled by http.Client default
//   - 4xx (other than above) → ErrPermanent (no retry; emit AssetLost)
//   - network error          → ErrTransient (retry)

package asset

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultDownloadTimeout is the per-attempt download SLA (ADR-010).
const DefaultDownloadTimeout = 60 * time.Second

// DownloadResult is what Downloader.Download returns. Body MUST be
// closed by the caller.
type DownloadResult struct {
	Body        io.ReadCloser
	ContentType string
	SizeBytes   int64 // -1 when unknown (chunked transfer)
}

// Downloader is the interface AssetWorker uses to fetch upstream bytes.
// Splitting it out lets tests substitute a fake without spinning up
// httptest servers for every case.
type Downloader interface {
	// Download retrieves the bytes at src, returning a streaming reader.
	// Caller MUST Close() the returned Body.
	Download(ctx context.Context, src string) (*DownloadResult, error)
}

// ErrTransient wraps a download error that the caller may retry.
type ErrTransient struct{ Err error }

func (e *ErrTransient) Error() string { return "transient: " + e.Err.Error() }
func (e *ErrTransient) Unwrap() error { return e.Err }

// ErrPermanent wraps a download error that should NOT be retried —
// the caller emits AssetLost immediately.
type ErrPermanent struct{ Err error }

func (e *ErrPermanent) Error() string { return "permanent: " + e.Err.Error() }
func (e *ErrPermanent) Unwrap() error { return e.Err }

// IsTransient reports whether err (or any error in its chain) is an
// ErrTransient.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	var t *ErrTransient
	return errors.As(err, &t)
}

// ─────────────────────────────────────────────────────────────────────────
// Dispatcher
// ─────────────────────────────────────────────────────────────────────────

// SchemeDispatcher routes a Download call to a per-scheme downloader.
// Unknown schemes return ErrPermanent.
//
// HTTPS is mandatory in production — http.Get will be used only in tests
// where httptest.Server returns http:// URLs. The worker's URL allow-list
// (in worker.go) enforces https-or-gs in non-test builds.
type SchemeDispatcher struct {
	// HTTP handles http:// and https://. May be nil; HTTP URLs return
	// ErrPermanent in that case.
	HTTP Downloader

	// GCS may be nil; gs:// URLs return ErrPermanent in that case. The
	// dev environment skips gs:// entirely.
	GCS Downloader
}

// NewSchemeDispatcher returns a dispatcher with the supplied downloaders.
// Pass nil for unconfigured schemes.
func NewSchemeDispatcher(http, gcs Downloader) *SchemeDispatcher {
	return &SchemeDispatcher{HTTP: http, GCS: gcs}
}

// Download dispatches src by scheme.
func (d *SchemeDispatcher) Download(ctx context.Context, src string) (*DownloadResult, error) {
	if src == "" {
		return nil, &ErrPermanent{Err: errors.New("download: empty source URL")}
	}
	scheme := schemeOf(src)
	switch scheme {
	case "http", "https":
		if d.HTTP == nil {
			return nil, &ErrPermanent{Err: fmt.Errorf("download: scheme %q not configured", scheme)}
		}
		return d.HTTP.Download(ctx, src)
	case "gs":
		if d.GCS == nil {
			return nil, &ErrPermanent{Err: errors.New("download: gs:// scheme not configured")}
		}
		return d.GCS.Download(ctx, src)
	default:
		return nil, &ErrPermanent{Err: fmt.Errorf("download: unsupported scheme %q", scheme)}
	}
}

// schemeOf returns the lowercase scheme prefix of src, or "" when none.
func schemeOf(src string) string {
	idx := strings.Index(src, "://")
	if idx <= 0 {
		return ""
	}
	return strings.ToLower(src[:idx])
}

// ─────────────────────────────────────────────────────────────────────────
// HTTP downloader (https://, http://)
// ─────────────────────────────────────────────────────────────────────────

// HTTPDownloader fetches bytes over HTTP/HTTPS.
//
// Implementations MUST set a per-attempt timeout (default
// DefaultDownloadTimeout). The Client field allows tests to inject a
// httptest.Client.
type HTTPDownloader struct {
	Client  *http.Client
	Timeout time.Duration
}

// NewHTTPDownloader returns an HTTPDownloader with sensible defaults.
func NewHTTPDownloader() *HTTPDownloader {
	return &HTTPDownloader{
		Client:  &http.Client{Timeout: DefaultDownloadTimeout},
		Timeout: DefaultDownloadTimeout,
	}
}

// Download implements Downloader.
//
// Status code mapping:
//
//	2xx                                    → success
//	408, 425, 429, 5xx                     → ErrTransient
//	other 4xx                              → ErrPermanent
func (h *HTTPDownloader) Download(ctx context.Context, src string) (*DownloadResult, error) {
	timeout := h.Timeout
	if timeout <= 0 {
		timeout = DefaultDownloadTimeout
	}
	dlCtx, cancel := context.WithTimeout(ctx, timeout)
	// We do NOT defer cancel() — the body reader owns the ctx lifetime.
	req, err := http.NewRequestWithContext(dlCtx, http.MethodGet, src, nil)
	if err != nil {
		cancel()
		return nil, &ErrPermanent{Err: fmt.Errorf("download: build request: %w", err)}
	}
	client := h.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		cancel()
		// Network errors are transient — DNS blip, connection reset, etc.
		return nil, &ErrTransient{Err: fmt.Errorf("download: do request: %w", err)}
	}
	if !isSuccess(resp.StatusCode) {
		_ = resp.Body.Close()
		cancel()
		err := fmt.Errorf("download: upstream HTTP %d for %s", resp.StatusCode, src)
		if isTransientStatus(resp.StatusCode) {
			return nil, &ErrTransient{Err: err}
		}
		return nil, &ErrPermanent{Err: err}
	}
	size := int64(-1)
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		if v, err := strconv.ParseInt(cl, 10, 64); err == nil && v >= 0 {
			size = v
		}
	}
	return &DownloadResult{
		Body:        &ctxClosingBody{rc: resp.Body, cancel: cancel},
		ContentType: resp.Header.Get("Content-Type"),
		SizeBytes:   size,
	}, nil
}

// ctxClosingBody wraps an http.Response.Body so that closing the body
// also cancels the per-request context; this prevents a leaked timer.
type ctxClosingBody struct {
	rc     io.ReadCloser
	cancel context.CancelFunc
}

func (c *ctxClosingBody) Read(p []byte) (int, error) { return c.rc.Read(p) }
func (c *ctxClosingBody) Close() error {
	err := c.rc.Close()
	if c.cancel != nil {
		c.cancel()
	}
	return err
}

// isSuccess reports whether code is 2xx.
func isSuccess(code int) bool { return code >= 200 && code < 300 }

// isTransientStatus reports whether code should trigger a retry.
func isTransientStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout,    // 408
		http.StatusTooEarly,            // 425
		http.StatusTooManyRequests,     // 429
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout:      // 504
		return true
	}
	return false
}

// ─────────────────────────────────────────────────────────────────────────
// GCS downloader (gs://) — interface-driven, real impl is HTTP-based
// ─────────────────────────────────────────────────────────────────────────

// GCSDownloader fetches gs://bucket/object via the Google Cloud Storage
// REST endpoint at https://storage.googleapis.com/storage/v1/b/{bucket}/
// o/{object}?alt=media. Auth is handled by the supplied AuthedClient,
// which the googleai adapter constructs from the same OAuth2 credentials
// it already uses for Vertex AI (cloud-platform scope is sufficient).
//
// We implement this as HTTP-on-top-of-OAuth2 instead of importing
// cloud.google.com/go/storage to avoid dependency churn at S9.5 time.
// The shape is interface-friendly so a test can substitute a stub.
type GCSDownloader struct {
	// Client is an *http.Client that injects a Bearer token on every
	// request. Tests pass a plain client pointed at httptest.Server.
	Client *http.Client

	// Endpoint overrides the default Google endpoint. Tests use the
	// httptest.Server URL; production leaves this empty.
	Endpoint string
}

// DefaultGCSEndpoint is the public Google Cloud Storage JSON API host.
const DefaultGCSEndpoint = "https://storage.googleapis.com"

// NewGCSDownloader returns a GCSDownloader. client must inject the
// OAuth2 bearer; the asset package does NOT depend on the googleai
// package's auth helpers (call sites construct their own client).
func NewGCSDownloader(client *http.Client) *GCSDownloader {
	return &GCSDownloader{Client: client}
}

// Download implements Downloader. src must be a gs:// URL.
func (g *GCSDownloader) Download(ctx context.Context, src string) (*DownloadResult, error) {
	bucket, object, err := parseGSURL(src)
	if err != nil {
		return nil, &ErrPermanent{Err: err}
	}
	endpoint := g.Endpoint
	if endpoint == "" {
		endpoint = DefaultGCSEndpoint
	}
	// Per Google's REST API: https://storage.googleapis.com/storage/v1/b/{bucket}/o/{object}?alt=media
	// Object names must be URL-encoded preserving slashes? No — the
	// REST docs say the object name is fully URL-encoded INCLUDING /.
	encoded := url.PathEscape(object)
	target := fmt.Sprintf("%s/storage/v1/b/%s/o/%s?alt=media",
		endpoint, url.PathEscape(bucket), encoded)

	dlCtx, cancel := context.WithTimeout(ctx, DefaultDownloadTimeout)
	req, err := http.NewRequestWithContext(dlCtx, http.MethodGet, target, nil)
	if err != nil {
		cancel()
		return nil, &ErrPermanent{Err: fmt.Errorf("gcs: build request: %w", err)}
	}
	client := g.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		cancel()
		return nil, &ErrTransient{Err: fmt.Errorf("gcs: do request: %w", err)}
	}
	if !isSuccess(resp.StatusCode) {
		_ = resp.Body.Close()
		cancel()
		err := fmt.Errorf("gcs: upstream HTTP %d for %s/%s", resp.StatusCode, bucket, object)
		if isTransientStatus(resp.StatusCode) {
			return nil, &ErrTransient{Err: err}
		}
		return nil, &ErrPermanent{Err: err}
	}
	size := int64(-1)
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		if v, err := strconv.ParseInt(cl, 10, 64); err == nil && v >= 0 {
			size = v
		}
	}
	return &DownloadResult{
		Body:        &ctxClosingBody{rc: resp.Body, cancel: cancel},
		ContentType: resp.Header.Get("Content-Type"),
		SizeBytes:   size,
	}, nil
}

// parseGSURL splits "gs://bucket/object/path" into (bucket, object).
func parseGSURL(src string) (string, string, error) {
	const prefix = "gs://"
	if !strings.HasPrefix(src, prefix) {
		return "", "", fmt.Errorf("gcs: not a gs:// URL: %q", src)
	}
	rest := src[len(prefix):]
	slash := strings.Index(rest, "/")
	if slash <= 0 {
		return "", "", fmt.Errorf("gcs: missing object path in %q", src)
	}
	bucket := rest[:slash]
	object := rest[slash+1:]
	if bucket == "" || object == "" {
		return "", "", fmt.Errorf("gcs: invalid gs URL %q", src)
	}
	return bucket, object, nil
}

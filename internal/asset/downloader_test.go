package asset

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSchemeDispatcher_Dispatch(t *testing.T) {
	t.Parallel()

	httpDl := newStubDownloader([]byte("https-bytes"), "image/png")
	gcsDl := newStubDownloader([]byte("gcs-bytes"), "video/mp4")
	d := NewSchemeDispatcher(httpDl, gcsDl)

	cases := []struct {
		name    string
		src     string
		want    string
		wantErr bool
		errIs   func(error) bool
	}{
		{"https", "https://example.com/x.png", "https-bytes", false, nil},
		{"http", "http://example.com/x.png", "https-bytes", false, nil},
		{"gs", "gs://bkt/obj.mp4", "gcs-bytes", false, nil},
		{"empty", "", "", true, func(err error) bool {
			var p *ErrPermanent
			return errors.As(err, &p)
		}},
		{"unsupported-scheme", "ftp://nope/file", "", true, func(err error) bool {
			var p *ErrPermanent
			return errors.As(err, &p)
		}},
		{"data-uri", "data:image/png;base64,abc", "", true, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := d.Download(context.Background(), tc.src)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got result %v", res)
				}
				if tc.errIs != nil && !tc.errIs(err) {
					t.Errorf("error type mismatch: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			defer res.Body.Close()
			data, _ := io.ReadAll(res.Body)
			if string(data) != tc.want {
				t.Errorf("body: want %q, got %q", tc.want, string(data))
			}
		})
	}
}

func TestSchemeDispatcher_NilDownloaders(t *testing.T) {
	t.Parallel()
	d := NewSchemeDispatcher(nil, nil)
	for _, src := range []string{"https://x", "gs://b/o"} {
		_, err := d.Download(context.Background(), src)
		if err == nil {
			t.Errorf("nil dispatcher must reject %s", src)
		}
		var p *ErrPermanent
		if !errors.As(err, &p) {
			t.Errorf("want ErrPermanent for %s, got %v", src, err)
		}
	}
}

func TestHTTPDownloader_Success(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Length", "9")
		_, _ = w.Write([]byte("imagebody"))
	}))
	defer srv.Close()

	dl := NewHTTPDownloader()
	res, err := dl.Download(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	defer res.Body.Close()
	if res.ContentType != "image/png" {
		t.Errorf("content type: %q", res.ContentType)
	}
	if res.SizeBytes != 9 {
		t.Errorf("size: %d", res.SizeBytes)
	}
	body, _ := io.ReadAll(res.Body)
	if string(body) != "imagebody" {
		t.Errorf("body: %q", string(body))
	}
}

func TestHTTPDownloader_PermanentError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusGone)
	}))
	defer srv.Close()

	dl := NewHTTPDownloader()
	_, err := dl.Download(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected error")
	}
	var perm *ErrPermanent
	if !errors.As(err, &perm) {
		t.Errorf("want ErrPermanent, got %v", err)
	}
}

func TestHTTPDownloader_TransientError(t *testing.T) {
	t.Parallel()
	for _, code := range []int{408, 429, 500, 502, 503, 504} {
		t.Run("status-"+http.StatusText(code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "transient", code)
			}))
			defer srv.Close()
			dl := NewHTTPDownloader()
			_, err := dl.Download(context.Background(), srv.URL)
			if !IsTransient(err) {
				t.Errorf("status %d: want transient, got %v", code, err)
			}
		})
	}
}

func TestHTTPDownloader_NetworkErrorIsTransient(t *testing.T) {
	t.Parallel()
	dl := NewHTTPDownloader()
	// Use an unroutable IP / unused port to provoke a network error.
	_, err := dl.Download(context.Background(), "http://127.0.0.1:1/nope")
	if err == nil {
		t.Fatal("expected error")
	}
	if !IsTransient(err) {
		t.Errorf("network error should be transient, got %v", err)
	}
}

func TestHTTPDownloader_ContextCancelled(t *testing.T) {
	t.Parallel()
	dl := NewHTTPDownloader()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := dl.Download(ctx, "http://127.0.0.1:1/nope")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestGCSDownloader_Success(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /storage/v1/b/{bucket}/o/{object}?alt=media — the URL
		// path-encodes the object including slashes.
		if !strings.HasPrefix(r.URL.Path, "/storage/v1/b/") {
			http.Error(w, "bad path: "+r.URL.Path, http.StatusBadRequest)
			return
		}
		if r.URL.Query().Get("alt") != "media" {
			http.Error(w, "missing alt=media", http.StatusBadRequest)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer fake-token" {
			http.Error(w, "missing auth: "+got, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Length", "8")
		_, _ = w.Write([]byte("mp4bytes"))
	}))
	defer srv.Close()

	gcs := NewGCSDownloader(&http.Client{
		Transport: &fakeAuthTransport{token: "fake-token"},
	})
	gcs.Endpoint = srv.URL
	res, err := gcs.Download(context.Background(), "gs://bkt/path/to/obj.mp4")
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	defer res.Body.Close()
	if res.ContentType != "video/mp4" {
		t.Errorf("content type: %q", res.ContentType)
	}
	body, _ := io.ReadAll(res.Body)
	if string(body) != "mp4bytes" {
		t.Errorf("body: %q", string(body))
	}
}

func TestGCSDownloader_BadGSURL(t *testing.T) {
	t.Parallel()
	gcs := NewGCSDownloader(&http.Client{})
	cases := []string{
		"https://not-gs/x",
		"gs://",
		"gs://bucket-only",
		"gs:///object-only",
	}
	for _, src := range cases {
		_, err := gcs.Download(context.Background(), src)
		if err == nil {
			t.Errorf("%s: expected error", src)
			continue
		}
		var perm *ErrPermanent
		if !errors.As(err, &perm) {
			t.Errorf("%s: want ErrPermanent, got %v", src, err)
		}
	}
}

func TestGCSDownloader_TransientStatus(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer srv.Close()
	gcs := NewGCSDownloader(&http.Client{Transport: &fakeAuthTransport{token: "x"}})
	gcs.Endpoint = srv.URL
	_, err := gcs.Download(context.Background(), "gs://b/o")
	if !IsTransient(err) {
		t.Errorf("502 should be transient, got %v", err)
	}
}

func TestGCSDownloader_PermanentStatus(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()
	gcs := NewGCSDownloader(&http.Client{Transport: &fakeAuthTransport{token: "x"}})
	gcs.Endpoint = srv.URL
	_, err := gcs.Download(context.Background(), "gs://b/o")
	if IsTransient(err) {
		t.Errorf("403 should be permanent, got %v", err)
	}
}

func TestSchemeOf(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"https://x":   "https",
		"HTTP://x":    "http",
		"gs://b/o":    "gs",
		"data:image":  "",
		"":            "",
		"no-scheme":   "",
	}
	for src, want := range cases {
		if got := schemeOf(src); got != want {
			t.Errorf("schemeOf(%q): want %q, got %q", src, want, got)
		}
	}
}

// ─── helpers ───

// stubDownloader is a Downloader that returns a fixed body.
type stubDownloader struct {
	body        []byte
	contentType string
	calls       atomic.Int64
}

func newStubDownloader(body []byte, ct string) *stubDownloader {
	return &stubDownloader{body: body, contentType: ct}
}

func (s *stubDownloader) Download(ctx context.Context, src string) (*DownloadResult, error) {
	s.calls.Add(1)
	return &DownloadResult{
		Body:        io.NopCloser(strings.NewReader(string(s.body))),
		ContentType: s.contentType,
		SizeBytes:   int64(len(s.body)),
	}, nil
}

// fakeAuthTransport injects a static bearer token.
type fakeAuthTransport struct {
	token string
}

func (f *fakeAuthTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r.Header.Set("Authorization", "Bearer "+f.token)
	return http.DefaultTransport.RoundTrip(r)
}

var _ time.Duration // keep time import used; some test paths use it via httptest defaults

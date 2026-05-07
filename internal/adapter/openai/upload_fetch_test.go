package openai

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/internal/adapter"
)

func TestFetchSourceImage_Happy(t *testing.T) {
	fetcher := newMemoryFetcher()
	fetcher.put("upload_abc", []byte("\x89PNG\r\n\x1a\nfake-bytes"), "image/png", "user.png")
	got, err := fetchSourceImage(context.Background(), fetcher, "upload_abc")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if string(got.Bytes) != "\x89PNG\r\n\x1a\nfake-bytes" {
		t.Errorf("bytes mismatch: %q", got.Bytes)
	}
	if got.MimeType != "image/png" {
		t.Errorf("mime = %q, want image/png", got.MimeType)
	}
	if got.Filename != "user.png" {
		t.Errorf("filename = %q, want user.png", got.Filename)
	}
}

func TestFetchSourceImage_EmptyID(t *testing.T) {
	fetcher := newMemoryFetcher()
	for _, id := range []string{"", "   ", "\t"} {
		_, err := fetchSourceImage(context.Background(), fetcher, id)
		if err == nil {
			t.Errorf("expected error for empty id %q", id)
		}
		if !errors.Is(err, adapter.ErrInvalidParams) {
			t.Errorf("err = %v, want ErrInvalidParams", err)
		}
	}
}

func TestFetchSourceImage_NilFetcher(t *testing.T) {
	_, err := fetchSourceImage(context.Background(), nil, "upload_abc")
	if err == nil {
		t.Fatal("expected error for nil fetcher")
	}
}

func TestFetchSourceImage_StorageMiss(t *testing.T) {
	fetcher := newMemoryFetcher()
	_, err := fetchSourceImage(context.Background(), fetcher, "upload_missing")
	if err == nil {
		t.Fatal("expected miss error")
	}
	if !errors.Is(err, ErrUploadNotFound) {
		t.Errorf("err = %v, want ErrUploadNotFound", err)
	}
}

func TestFetchSourceImage_OversizedRejected(t *testing.T) {
	fetcher := newMemoryFetcher()
	// MaxSourceImageBytes + 1
	huge := make([]byte, MaxSourceImageBytes+1)
	for i := range huge {
		huge[i] = 0xFF
	}
	fetcher.put("upload_huge", huge, "image/png", "huge.png")
	_, err := fetchSourceImage(context.Background(), fetcher, "upload_huge")
	if err == nil {
		t.Fatal("expected oversize rejection")
	}
	if !strings.Contains(err.Error(), "AP-17") {
		t.Errorf("err should reference AP-17: %v", err)
	}
}

func TestFetchSourceImage_EmptyPayload(t *testing.T) {
	fetcher := newMemoryFetcher()
	fetcher.put("upload_empty", []byte{}, "image/png", "x.png")
	_, err := fetchSourceImage(context.Background(), fetcher, "upload_empty")
	if err == nil {
		t.Fatal("expected error for empty payload")
	}
}

func TestFetchSourceImage_DisallowedMime(t *testing.T) {
	cases := []string{"image/svg+xml", "image/gif", "text/html", "application/pdf", ""}
	for _, mime := range cases {
		t.Run(mime, func(t *testing.T) {
			fetcher := newMemoryFetcher()
			fetcher.put("upload_bad", []byte("not-image"), mime, "x")
			_, err := fetchSourceImage(context.Background(), fetcher, "upload_bad")
			if err == nil {
				t.Errorf("expected mime-rejection error for %q", mime)
			}
		})
	}
}

func TestFetchSourceImage_StorageError(t *testing.T) {
	fetcher := newMemoryFetcher()
	fetcher.putError("upload_brk", errors.New("backend unreachable"))
	_, err := fetchSourceImage(context.Background(), fetcher, "upload_brk")
	if err == nil {
		t.Fatal("expected wrapped storage error")
	}
}

func TestFetchSourceImage_ContextCanceled(t *testing.T) {
	fetcher := newMemoryFetcher()
	fetcher.put("upload_abc", []byte("data"), "image/png", "x.png")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := fetchSourceImage(ctx, fetcher, "upload_abc")
	if err == nil {
		t.Fatal("expected context error")
	}
}

func TestSanitizeFilename(t *testing.T) {
	cases := []struct {
		in   string
		mime string
		want string
	}{
		{"clean.png", "image/png", "clean.png"},
		{"", "image/png", "source.png"},
		{"", "image/jpeg", "source.jpg"},
		{"", "image/webp", "source.webp"},
		{"", "application/octet-stream", "source.bin"},
		{"../../etc/passwd", "image/png", "____etc_passwd"},
		{"a/b\\c", "image/png", "a_b_c"},
	}
	for _, tc := range cases {
		t.Run(tc.in+"|"+tc.mime, func(t *testing.T) {
			got := sanitizeFilename(tc.in, tc.mime)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMemoryFetcher_RoundTrip(t *testing.T) {
	mf := newMemoryFetcher()
	mf.put("a", []byte("hello"), "image/png", "f.png")
	rc, mime, fname, err := mf.Fetch(context.Background(), "a")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer rc.Close()
	body, _ := io.ReadAll(rc)
	if string(body) != "hello" {
		t.Errorf("body = %q", body)
	}
	if mime != "image/png" || fname != "f.png" {
		t.Errorf("mime/fname = %s/%s", mime, fname)
	}
}

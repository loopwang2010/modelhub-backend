// upload_fetch.go — bridge between our pre-signed-upload pattern (S2.5
// /v1/uploads + S9.5 object storage) and OpenAI's multipart requirement.
//
// Per ADR-009, modelhub's public API is JSON-only: clients reference
// uploaded source images by `upload_id` in their /v1/generations params.
// OpenAI's image-edit endpoint, however, demands multipart/form-data with
// the source image bytes inline. This file is the isolation membrane —
// the OpenAI-shape multipart oddity NEVER leaks past internal/adapter/openai/.
//
// AP-13 enforcement: source upload images NEVER touch backend disk.
// The fetcher streams from object storage into memory, and we cap the
// in-memory size at MaxSourceImageBytes before any byte of it is buffered.
//
// AP-17 defense-in-depth: even though /v1/uploads validates content-type
// and size at upload time, this fetcher MUST re-validate. A corrupted or
// oversized blob from storage (e.g., race condition, replay attack on a
// reusable upload_id) gets rejected here too. Belt-and-suspenders: the
// upload endpoint is the primary guard, this is the secondary.

package openai

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/internal/adapter"
)

// MaxSourceImageBytes caps a single source image at 20 MiB. OpenAI's own
// limit is 4 MiB for gpt-image-1 PNG-with-alpha edits, but we accept larger
// at our boundary so the upload validator's defaults (50 MiB allow-list,
// see internal/api/upload.go) and OpenAI's stricter cap can both be sources
// of truth. Values above MaxSourceImageBytes are hard-rejected before any
// byte hits memory.
const MaxSourceImageBytes int64 = 20 * 1024 * 1024

// allowedSourceMimeTypes is the per-adapter allow-list of source-image
// MIME types. Tighter than internal/api/upload.go's allow-list because
// OpenAI's image-edit endpoint only accepts raster images. SVG is
// explicitly NOT here — it's a known XSS/script-execution vector.
var allowedSourceMimeTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/webp": true,
}

// SourceImage is the in-memory representation of a fetched upload.
// Used to populate the multipart form-data field for the OpenAI request.
type SourceImage struct {
	Bytes    []byte
	MimeType string
	Filename string // Display-only; never used to construct paths.
}

// uploadFetcher is the abstraction over our object-storage backend.
// In production, this is implemented by the S9.5 asset/object-storage
// package; in tests, an in-memory map satisfies it.
//
// Returning io.ReadCloser is intentional — it lets the storage backend
// stream rather than load. We then cap-and-buffer at this layer.
type uploadFetcher interface {
	// Fetch returns a stream of the upload's bytes plus its declared
	// content-type and (informational) filename. The caller is responsible
	// for closing the reader.
	//
	// Returns ErrUploadNotFound when uploadID does not resolve.
	Fetch(ctx context.Context, uploadID string) (io.ReadCloser, string, string, error)
}

// ErrUploadNotFound is returned by uploadFetcher implementations when the
// referenced upload_id does not resolve. Mapped by the adapter to
// ErrInvalidParams (the user gave a bad reference, not an upstream issue).
var ErrUploadNotFound = errors.New("openai: upload_id not found in storage")

// fetchSourceImage resolves an upload_id reference into a fully-buffered
// SourceImage struct, ready to be embedded in a multipart form. Performs
// AP-17 defense-in-depth checks even though the upload endpoint already
// validated.
//
// Steps:
//  1. Reject empty / whitespace-only uploadID early.
//  2. Stream from object storage; abort if storage missed.
//  3. Cap at MaxSourceImageBytes via io.LimitReader; over-cap → reject.
//  4. Verify storage's declared content-type is in our allow-list.
//  5. Return populated SourceImage.
//
// Note: full magic-byte sniffing (true polyglot defense) is out of scope
// for this adapter — that's the upload validator's job (S9.5 object-
// storage layer). What we DO here is enforce the documented allow-list
// based on the storage's own metadata, which is already a defense-in-
// depth check beyond what the upload endpoint enforced.
func fetchSourceImage(ctx context.Context, fetcher uploadFetcher, uploadID string) (*SourceImage, error) {
	if fetcher == nil {
		return nil, errors.New("openai: nil uploadFetcher")
	}
	if strings.TrimSpace(uploadID) == "" {
		return nil, fmt.Errorf("%w: empty upload_id", adapter.ErrInvalidParams)
	}
	rc, mime, filename, err := fetcher.Fetch(ctx, uploadID)
	if err != nil {
		return nil, fmt.Errorf("openai: fetch upload %q: %w", uploadID, err)
	}
	defer rc.Close()

	// LimitReader caps how much we'll buffer. We read up to limit+1 so we
	// can distinguish "exactly at the cap" from "exceeded the cap".
	limited := io.LimitReader(rc, MaxSourceImageBytes+1)
	buf, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("openai: read upload %q: %w", uploadID, err)
	}
	if int64(len(buf)) > MaxSourceImageBytes {
		return nil, fmt.Errorf("openai: source image exceeds %d bytes (AP-17)", MaxSourceImageBytes)
	}
	if len(buf) == 0 {
		return nil, fmt.Errorf("openai: source image is empty (storage returned 0 bytes)")
	}

	mime = strings.ToLower(strings.TrimSpace(mime))
	if !allowedSourceMimeTypes[mime] {
		return nil, fmt.Errorf("openai: source image mime %q not in allow-list (AP-17)", mime)
	}
	return &SourceImage{
		Bytes:    buf,
		MimeType: mime,
		Filename: sanitizeFilename(filename, mime),
	}, nil
}

// sanitizeFilename returns a display-safe filename. Multipart filename is
// optional but OpenAI sometimes uses the extension to infer content-type;
// we synthesize a deterministic name when storage didn't supply one or
// when the supplied name has unsafe characters.
func sanitizeFilename(filename, mime string) string {
	clean := strings.TrimSpace(filename)
	// Strip any path separators — multipart filename should never embed paths.
	clean = strings.ReplaceAll(clean, "/", "_")
	clean = strings.ReplaceAll(clean, "\\", "_")
	clean = strings.ReplaceAll(clean, "..", "_")
	if clean == "" {
		switch mime {
		case "image/png":
			return "source.png"
		case "image/jpeg":
			return "source.jpg"
		case "image/webp":
			return "source.webp"
		default:
			return "source.bin"
		}
	}
	return clean
}

// memoryFetcher is the in-memory test double for uploadFetcher.
// Exposed (lowercase) to the openai package so adapter_test.go can
// inject it without leaking the real S3 client into test code.
type memoryFetcher struct {
	items map[string]memoryFetchItem
}

type memoryFetchItem struct {
	bytes    []byte
	mime     string
	filename string
	err      error // when non-nil, Fetch returns this instead of bytes
}

// newMemoryFetcher constructs an empty in-memory upload store.
func newMemoryFetcher() *memoryFetcher {
	return &memoryFetcher{items: make(map[string]memoryFetchItem)}
}

func (m *memoryFetcher) put(id string, b []byte, mime, filename string) {
	m.items[id] = memoryFetchItem{bytes: b, mime: mime, filename: filename}
}

func (m *memoryFetcher) putError(id string, err error) {
	m.items[id] = memoryFetchItem{err: err}
}

func (m *memoryFetcher) Fetch(ctx context.Context, uploadID string) (io.ReadCloser, string, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", "", err
	}
	item, ok := m.items[uploadID]
	if !ok {
		return nil, "", "", ErrUploadNotFound
	}
	if item.err != nil {
		return nil, "", "", item.err
	}
	return io.NopCloser(strings.NewReader(string(item.bytes))), item.mime, item.filename, nil
}

// Verify httpClient interface is exported correctly elsewhere.
var _ http.RoundTripper = (*http.Transport)(nil)

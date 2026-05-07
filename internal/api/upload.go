// POST /v1/uploads — pre-signed PUT URL stub per ADR-009.
//
// Per ADR-009, multipart uploads do NOT happen on /v1/generations. Instead:
//
//  1. Client POSTs /v1/uploads with content-type + size, gets back an
//     upload_id and a pre-signed PUT URL.
//  2. Client uploads bytes directly to the pre-signed URL.
//  3. Client references upload_id in their /v1/generations params.
//
// This file is the request-side stub. The real S3-compatible signing
// backend lands in S9.5. For now this returns a fake URL so the frontend
// (S10) can wire its upload flow against a stable shape.
//
// AP guards enforced:
//   - AP-17 anti-pattern (upload size cap): hard-rejects requests above
//     MaxUploadBytes. Real signing must enforce the same cap server-side.
//   - Unknown content-types are rejected outright; the allow-list is small
//     and explicit. Adding a type is a deliberate decision.

package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// MaxUploadBytes caps a single upload at 50 MiB. This is an arbitrary but
// pragmatic limit for image/audio inputs in Sprint 1; raise once we onboard
// large-video edit modalities and have storage costs to back it.
const MaxUploadBytes int64 = 50 * 1024 * 1024

// allowedUploadContentTypes is the explicit allow-list. Updates require a
// security review — uploaders that accept arbitrary content-types are a
// classic XSS/SVG bomb vector.
var allowedUploadContentTypes = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/webp": true,
	"image/gif":  true,
	"audio/mpeg": true,
	"audio/wav":  true,
	"audio/mp3":  true,
	"audio/ogg":  true,
}

// UploadRequest is the wire shape for POST /v1/uploads.
type UploadRequest struct {
	// ContentType is the MIME type of the upload. Must be on the allow-list.
	ContentType string `json:"content_type"`
	// SizeBytes is the file size. Must be > 0 and ≤ MaxUploadBytes.
	SizeBytes int64 `json:"size_bytes"`
	// Filename is optional; informational only — we never use the client-
	// supplied filename to construct paths (path-traversal hardening).
	Filename string `json:"filename,omitempty"`
}

// Validate checks structural invariants.
func (r *UploadRequest) Validate() error {
	if r.ContentType == "" {
		return errors.New("upload: content_type is required")
	}
	if !allowedUploadContentTypes[strings.ToLower(r.ContentType)] {
		return fmt.Errorf("upload: content_type %q is not allowed", r.ContentType)
	}
	if r.SizeBytes <= 0 {
		return errors.New("upload: size_bytes must be positive")
	}
	if r.SizeBytes > MaxUploadBytes {
		return fmt.Errorf("upload: size_bytes %d exceeds MaxUploadBytes %d (AP-17)", r.SizeBytes, MaxUploadBytes)
	}
	return nil
}

// UploadResponse is what POST /v1/uploads returns. Clients use UploadURL
// to PUT bytes, then reference UploadID in subsequent /v1/generations
// params (e.g. {"image_id": "upload_xxx"}).
type UploadResponse struct {
	// UploadID is the opaque handle the gateway will resolve when generating.
	UploadID string `json:"upload_id"`

	// UploadURL is the pre-signed PUT URL. Single-use; expires per ExpiresAt.
	// In the stub, this is a fake URL on cdn.modelhub.local pointing at
	// the eventual S3 bucket. AP-19 is preserved — never an upstream URL.
	UploadURL string `json:"upload_url"`

	// Method is always "PUT" for stub; future revs may add multipart.
	Method string `json:"method"`

	// ExpiresAt is when the pre-signed URL stops working. The stub uses 15
	// minutes; production may shorten.
	ExpiresAt string `json:"expires_at"`

	// Headers is the set of headers the client MUST send with the upload.
	// Includes Content-Type and Content-Length minimally.
	Headers map[string]string `json:"headers"`
}

// CreateUpload is the HTTP handler for POST /v1/uploads.
//
// Stub behavior: validates the request, mints a fake upload ID, and returns
// a fake URL pointing at our CDN. Real S3 signing wiring lands in S9.5;
// this is intentionally minimal so client code can be written and tested
// against the stable shape now.
func CreateUpload() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
			return
		}
		var req UploadRequest
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_json", "request body is not valid JSON")
			return
		}
		if err := req.Validate(); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		resp, err := buildStubUploadResponse(req)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to mint upload URL")
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// buildStubUploadResponse generates a deterministic-ish fake response.
// Pulled out so tests can assert against shape without HTTP wiring.
func buildStubUploadResponse(req UploadRequest) (*UploadResponse, error) {
	id, err := mintUploadID()
	if err != nil {
		return nil, err
	}
	// CDN-shaped URL — AP-19 guard.
	url := fmt.Sprintf("https://cdn.modelhub.local/uploads/%s", id)
	return &UploadResponse{
		UploadID:  id,
		UploadURL: url,
		Method:    "PUT",
		// 15 minutes from now in RFC3339, computed from the request's
		// timestamp. Tests can pass a fixed clock; for now we just emit
		// "stub" so production wiring (S9.5) is the only place that
		// emits a real expiry.
		ExpiresAt: "stub-15m",
		Headers: map[string]string{
			"Content-Type":   req.ContentType,
			"Content-Length": fmt.Sprintf("%d", req.SizeBytes),
		},
	}, nil
}

// mintUploadID generates a 16-byte random hex ID prefixed "upload_".
func mintUploadID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return "upload_" + hex.EncodeToString(buf[:]), nil
}

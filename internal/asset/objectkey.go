// BuildObjectKey — deterministic object-key minting for stored assets.
//
// Path layout: outputs/{task_id}/{filename}
//
// Note: BLUEPRINT.md §S9.5 spec says outputs/{user_id}/{task_id}/{filename}
// but the OutputAvailable event does not carry account_id (the FSM event
// vocabulary in events/task_events.go is locked). The asset worker has
// only TaskID at hand. We use outputs/{task_id}/... and document this as
// open question §3 — a follow-up could enrich the event with AccountID.
//
// Filename rules:
//   - Stable across re-uploads: same task_id + same upstream_url + same
//     mime → same filename. Idempotent re-runs do not produce duplicate
//     storage objects with different names.
//   - Extension-aware: derived from MIME via the local table; falls back
//     to ".bin" for unknown types so the URL still has a sane suffix.
//   - Hashed input: keeps the filename short and prevents path-traversal
//     bytes from upstream URLs from leaking into the storage path.

package asset

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// BuildObjectKey returns the storage key for a given task + upstream URL
// + mime type triple. Deterministic — repeat invocations produce the
// same key.
func BuildObjectKey(taskID, upstreamURL, mime string) string {
	if taskID == "" {
		taskID = "unknown-task"
	}
	hash := sha256.Sum256([]byte(taskID + "\x00" + upstreamURL))
	short := hex.EncodeToString(hash[:8])
	ext := extensionFor(mime)
	return "outputs/" + sanitizeTaskID(taskID) + "/" + short + ext
}

// extensionFor returns a file extension (with leading dot) for the given
// MIME type. Returns ".bin" when the MIME is unknown or empty.
//
// Conservative table: only MIMEs we expect from real upstream providers
// (image/*, video/*, audio/*) are mapped.
func extensionFor(mime string) string {
	mime = strings.ToLower(strings.TrimSpace(mime))
	if idx := strings.Index(mime, ";"); idx >= 0 {
		mime = strings.TrimSpace(mime[:idx])
	}
	switch mime {
	case "image/png":
		return ".png"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	case "video/mp4":
		return ".mp4"
	case "video/webm":
		return ".webm"
	case "audio/mpeg", "audio/mp3":
		return ".mp3"
	case "audio/wav", "audio/x-wav":
		return ".wav"
	case "audio/ogg":
		return ".ogg"
	case "audio/flac":
		return ".flac"
	case "text/plain":
		return ".txt"
	case "application/json":
		return ".json"
	default:
		return ".bin"
	}
}

// sanitizeTaskID strips characters that would be unsafe in a storage
// path. Task IDs from internal/task are always "gen_<hex>" — strict —
// but defense-in-depth keeps the storage layer safe even if a future
// caller passes garbage.
func sanitizeTaskID(id string) string {
	var b strings.Builder
	b.Grow(len(id))
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}

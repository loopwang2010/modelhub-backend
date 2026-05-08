// Webhook receiver — POST /v1/webhooks/{provider}/{task_id}/{token}
//
// Per S5-WORKER-DESIGN.md §5 and AP-18: the URL embeds an unguessable
// 256-bit per-task `webhook_token`. Lookup is by token (not task_id) so
// task-id enumeration cannot reach the handler. HMAC verification via
// adapter.VerifyWebhook is mandatory; tampered or missing signatures
// return 401.
//
// Idempotency: AssertTransition in the FSM rejects re-entry into a
// terminal state, so a webhook delivered 3 times produces ONE state
// advance and 2 silent no-ops (handler returns 200 each time).
//
// Status codes:
//   404 — token does not match any task; OR provider key has no adapter;
//         OR provider in URL ≠ provider stored on task. Single 404 makes
//         token enumeration indistinguishable from task absence.
//   400 — webhook references a different upstream_ref than our stored ref
//   401 — HMAC verification failed
//   405 — adapter does not support webhooks
//   200 — accepted (whether the FSM actually moved or was already
//         past this point — both are "your delivery worked")

package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/internal/adapter"
)

// WebhookTaskRow is the projection of `task` columns the webhook handler
// reads. Decouples the handler from internal/task.Task so the API
// package doesn't depend on the persistence layer.
type WebhookTaskRow struct {
	ID          string
	State       string
	UpstreamRef string
	HeldAmount  int64
	ProviderKey string
}

// WebhookTaskLookup is the small interface satisfied by
// internal/task.Repo (via a thin adapter). Defined here per accept-
// interfaces-return-structs.
type WebhookTaskLookup interface {
	FindByWebhookToken(ctx context.Context, token string) (*WebhookTaskRow, error)
}

// WebhookAdvancer applies a verified webhook to the FSM.
type WebhookAdvancer interface {
	AdvanceFromWebhook(ctx context.Context, taskID string, verif *adapter.WebhookVerification) error
}

// WebhookHandler builds the HTTP handler. The path is expected to be of
// the form `{prefix}/{provider}/{task_id}/{token}`.
//
// pathPrefix is the route prefix that should be stripped before parsing
// the {provider}/{task_id}/{token} suffix (e.g., "/v1/webhooks/").
//
// repo, registry, advancer are injected so the handler can be unit-tested
// without spinning up the full DB.
func WebhookHandler(pathPrefix string, repo WebhookTaskLookup, registry AdapterRegistry, advancer WebhookAdvancer) http.HandlerFunc {
	if repo == nil {
		panic("api: WebhookHandler requires non-nil repo")
	}
	if registry == nil {
		panic("api: WebhookHandler requires non-nil registry")
	}
	if advancer == nil {
		panic("api: WebhookHandler requires non-nil advancer")
	}
	if pathPrefix == "" {
		pathPrefix = "/v1/webhooks/"
	}
	if !strings.HasSuffix(pathPrefix, "/") {
		pathPrefix += "/"
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
			return
		}
		provider, taskID, token, ok := parseWebhookPath(r.URL.Path, pathPrefix)
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		t, err := repo.FindByWebhookToken(r.Context(), token)
		if err != nil || t == nil || t.ID != taskID {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if t.ProviderKey != provider {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		adp, ok := registry.Get(adapter.ProviderKey(provider))
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad_request", "failed to read body")
			return
		}
		verif, err := adp.VerifyWebhook(r.Header, body)
		if err != nil {
			if errors.Is(err, adapter.ErrUnsupported) {
				writeJSONError(w, http.StatusMethodNotAllowed, "webhook_unsupported", "this provider does not support webhooks")
				return
			}
			writeJSONError(w, http.StatusUnauthorized, "webhook_unauthorized", "signature verification failed")
			return
		}
		if verif == nil {
			writeJSONError(w, http.StatusUnauthorized, "webhook_unauthorized", "verification returned no result")
			return
		}
		if string(verif.UpstreamRef) != t.UpstreamRef {
			writeJSONError(w, http.StatusBadRequest, "webhook_ref_mismatch", "webhook upstream_ref does not match stored ref")
			return
		}
		if err := advancer.AdvanceFromWebhook(r.Context(), t.ID, verif); err != nil {
			if isIdempotentRejection(err) {
				w.WriteHeader(http.StatusOK)
				return
			}
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "advance failed")
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

// parseWebhookPath extracts the {provider, task_id, token} triplet.
// Returns (provider, taskID, token, true) on success.
func parseWebhookPath(p, prefix string) (string, string, string, bool) {
	if !strings.HasPrefix(p, prefix) {
		return "", "", "", false
	}
	rest := strings.TrimPrefix(p, prefix)
	rest = strings.Trim(rest, "/")
	parts := strings.Split(rest, "/")
	if len(parts) != 3 {
		return "", "", "", false
	}
	provider := parts[0]
	taskID := parts[1]
	token := parts[2]
	if provider == "" || taskID == "" || token == "" {
		return "", "", "", false
	}
	if strings.Contains(token, "..") || strings.Contains(taskID, "..") {
		return "", "", "", false
	}
	if !isProviderKey(provider) {
		return "", "", "", false
	}
	return provider, taskID, token, true
}

// isProviderKey returns true for characters allowed in a registry key.
func isProviderKey(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}

// isIdempotentRejection identifies "task already advanced past us"
// errors via their canonical message prefix. Matching by string keeps
// the api package free of an internal/task import cycle.
func isIdempotentRejection(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "illegal state transition") ||
		strings.Contains(msg, "already in terminal state")
}

// Glue: adapt task.Repo to the api.WebhookTaskLookup interface without
// creating an api → task import cycle.
//
// The api package declares its own minimal projection (api.WebhookTaskRow)
// and lookup interface; this file produces a value satisfying that
// interface from a *task.Repo.

package task

import (
	"context"

	"github.com/QuantumNous/new-api/internal/api"
)

// WebhookLookup adapts *Repo to api.WebhookTaskLookup. The wrapper is
// trivial — it forwards FindByWebhookToken and projects Task into
// api.WebhookTaskRow.
type WebhookLookup struct {
	Repo *Repo
}

// NewWebhookLookup constructs a WebhookLookup.
func NewWebhookLookup(r *Repo) *WebhookLookup { return &WebhookLookup{Repo: r} }

// FindByWebhookToken implements api.WebhookTaskLookup.
func (l *WebhookLookup) FindByWebhookToken(ctx context.Context, token string) (*api.WebhookTaskRow, error) {
	t, err := l.Repo.FindByWebhookToken(ctx, token)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, nil
	}
	return &api.WebhookTaskRow{
		ID:          t.ID,
		State:       string(t.State),
		UpstreamRef: t.UpstreamRef,
		HeldAmount:  int64(t.HeldAmount),
		ProviderKey: string(t.ProviderKey),
	}, nil
}

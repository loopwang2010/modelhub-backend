// Webhook FSM advancer — translates a verified webhook into FSM
// transitions, mirroring what the worker's advance() does for a Poll
// result.
//
// Idempotency comes from the FSM transition assertions: a 3x replay
// produces ONE successful state advance and 2 ErrIllegalTransition
// rejections. The HTTP handler treats those rejections as 200 (delivery
// worked) rather than errors.

package task

import (
	"context"

	"github.com/QuantumNous/new-api/internal/adapter"
	"github.com/QuantumNous/new-api/internal/events"
)

// WebhookAdvancer applies a verified webhook to a task's FSM. Returns
// the canonical task.ErrIllegalTransition / task.ErrTerminalState
// errors when the webhook arrived after the task already advanced —
// the HTTP layer treats those as 200 OK.
type WebhookAdvancer struct {
	Repo *Repo
	Bus  events.EventBus
}

// NewWebhookAdvancer constructs a WebhookAdvancer.
func NewWebhookAdvancer(repo *Repo, bus events.EventBus) *WebhookAdvancer {
	if repo == nil {
		panic("task: NewWebhookAdvancer requires non-nil repo")
	}
	if bus == nil {
		panic("task: NewWebhookAdvancer requires non-nil bus")
	}
	return &WebhookAdvancer{Repo: repo, Bus: bus}
}

// AdvanceFromWebhook applies verif.Result to the task identified by
// taskID. Mirrors Worker.advance() for the success / failure paths.
func (a *WebhookAdvancer) AdvanceFromWebhook(ctx context.Context, taskID string, verif *adapter.WebhookVerification) error {
	if verif == nil {
		return nil
	}
	t, err := a.Repo.FindByID(ctx, taskID)
	if err != nil {
		return err
	}
	if t == nil {
		return ErrTaskNotFound
	}
	pr := verif.Result
	switch pr.Status {
	case adapter.PollPending:
		return nil
	case adapter.PollRunning:
		if t.State == StateSubmitted {
			if err := a.Repo.MarkRunning(ctx, t.ID); err != nil {
				return err
			}
			_ = a.Bus.Publish(events.MakeTaskRunning(events.NewBaseEvent(events.TaskID(t.ID)), pr.Progress))
		}
		return nil
	case adapter.PollSucceeded:
		actual := t.HeldAmount
		if err := a.Repo.MarkSucceeded(ctx, t.ID, actual); err != nil {
			return err
		}
		_ = a.Bus.Publish(events.MakeTaskSucceeded(
			events.NewBaseEvent(events.TaskID(t.ID)),
			events.CostUSD(actual),
		))
		if pr.Result != nil && len(pr.Result.Outputs) > 0 {
			first := pr.Result.Outputs[0]
			_ = a.Bus.Publish(events.MakeOutputAvailable(
				events.NewBaseEvent(events.TaskID(t.ID)),
				first.URL, first.MimeType, first.SizeBytes,
			))
		}
		return nil
	case adapter.PollFailed:
		class := adapter.ErrClassUnknown
		msg := "upstream reported failure"
		var raw []byte
		if pr.Error != nil {
			if pr.Error.Class != "" {
				class = pr.Error.Class
			}
			if pr.Error.Message != "" {
				msg = pr.Error.Message
			}
			raw = pr.Error.Raw
		}
		if class == adapter.ErrClassTimeout {
			if err := a.Repo.MarkTimedOut(ctx, t.ID, msg); err != nil {
				return err
			}
			_ = a.Bus.Publish(events.MakeTaskTimedOut(
				events.NewBaseEvent(events.TaskID(t.ID)),
				events.CostUSD(t.HeldAmount),
			))
			return nil
		}
		if err := a.Repo.MarkFailed(ctx, t.ID, class, msg, raw); err != nil {
			return err
		}
		_ = a.Bus.Publish(events.MakeTaskFailed(
			events.NewBaseEvent(events.TaskID(t.ID)),
			events.ErrorClass(class), msg,
		))
		return nil
	}
	return nil
}

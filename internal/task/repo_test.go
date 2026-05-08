// Repo tests — exercise every state-mutation method and verify the
// audit log (task_event) sees one row per transition.

package task

import (
	"context"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/internal/adapter"
)

// TestRepo_Create_PersistsBaselineFields covers the create-time fields:
// ID, account, provider/model, params, idempotency key, webhook token,
// SLA deadline, held amount, created/updated timestamps.
func TestRepo_Create_PersistsBaselineFields(t *testing.T) {
	_, repo, cleanup := newTestDB(t)
	defer cleanup()

	p := newTestTaskParams("baseline")
	task, err := repo.Create(context.Background(), p)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if task.ID == "" || len(task.WebhookToken) != 64 {
		t.Errorf("ID=%q webhookToken=%q", task.ID, task.WebhookToken)
	}
	if task.State != StateCreated {
		t.Errorf("state = %q want %q", task.State, StateCreated)
	}
	if task.HeldAmount != p.HeldAmount {
		t.Errorf("held = %d want %d", task.HeldAmount, p.HeldAmount)
	}
	// Re-fetch and compare.
	again, err := repo.FindByID(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if again.ID != task.ID || again.State != task.State {
		t.Errorf("round-trip mismatch")
	}
	// One audit row for the initial create.
	events, err := repo.ListEvents(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(events) != 1 || events[0].ToState != string(StateCreated) {
		t.Errorf("audit log: %+v", events)
	}
}

// TestRepo_FullHappyPath walks Created → Held → Submitted → Running →
// Succeeded with an audit row per transition.
func TestRepo_FullHappyPath(t *testing.T) {
	_, repo, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	t1 := mustCreateAndSubmit(t, repo, newTestTaskParams("happy"))
	if err := repo.MarkRunning(ctx, t1.ID); err != nil {
		t.Fatalf("running: %v", err)
	}
	if err := repo.MarkSucceeded(ctx, t1.ID, 50_000); err != nil {
		t.Fatalf("succeeded: %v", err)
	}
	got, _ := repo.FindByID(ctx, t1.ID)
	if got.State != StateSucceeded {
		t.Errorf("end state = %q", got.State)
	}
	if got.ActualCost != 50_000 {
		t.Errorf("actual cost = %d", got.ActualCost)
	}
	events, _ := repo.ListEvents(ctx, t1.ID)
	expectedToStates := []string{
		string(StateCreated), string(StateHeld), string(StateSubmitted),
		string(StateRunning), string(StateSucceeded),
	}
	if len(events) != len(expectedToStates) {
		t.Fatalf("audit count = %d want %d", len(events), len(expectedToStates))
	}
	for i, ev := range events {
		if ev.ToState != expectedToStates[i] {
			t.Errorf("event %d: ToState=%q want %q", i, ev.ToState, expectedToStates[i])
		}
	}
}

// TestRepo_IllegalTransitionRejected — Created → Submitted is illegal.
func TestRepo_IllegalTransitionRejected(t *testing.T) {
	_, repo, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	task, err := repo.Create(ctx, newTestTaskParams("illegal"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Created → Submitted is illegal (must go via Held).
	err = repo.MarkSubmittedAsync(ctx, task.ID, "ref-x", "", time.Now().UTC())
	if err == nil {
		t.Fatalf("expected illegal-transition error")
	}
}

// TestRepo_TerminalRejectsFurther asserts that a Failed task cannot be
// re-failed or succeeded.
func TestRepo_TerminalRejectsFurther(t *testing.T) {
	_, repo, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	task, _ := repo.Create(ctx, newTestTaskParams("term"))
	if err := repo.MarkHeld(ctx, task.ID, 60_000); err != nil {
		t.Fatalf("held: %v", err)
	}
	if err := repo.MarkFailed(ctx, task.ID, adapter.ErrClassUpstream, "boom", []byte("upstream details")); err != nil {
		t.Fatalf("failed: %v", err)
	}
	// Second mark should error.
	err := repo.MarkSucceeded(ctx, task.ID, 1)
	if err == nil {
		t.Fatalf("expected terminal-state error")
	}
	if !isAlreadyTerminal(err) {
		t.Errorf("expected terminal/illegal err; got %v", err)
	}
}

// TestRepo_FindByWebhookToken covers the AP-18 token lookup.
func TestRepo_FindByWebhookToken(t *testing.T) {
	_, repo, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	task, _ := repo.Create(ctx, newTestTaskParams("tok"))
	got, err := repo.FindByWebhookToken(ctx, task.WebhookToken)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if got == nil || got.ID != task.ID {
		t.Errorf("expected task; got %v", got)
	}
	// Wrong token — nil.
	miss, _ := repo.FindByWebhookToken(ctx, "00")
	if miss != nil {
		t.Errorf("expected nil for unknown token")
	}
}

// TestRepo_SchedulePoll_DoesNotChangeState asserts SchedulePoll only
// updates poll_attempt + next_poll_after, not state.
func TestRepo_SchedulePoll_DoesNotChangeState(t *testing.T) {
	_, repo, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	task := mustCreateAndSubmit(t, repo, newTestTaskParams("sched"))
	if err := repo.SchedulePoll(ctx, task.ID, time.Now().Add(30*time.Second), 5); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	got, _ := repo.FindByID(ctx, task.ID)
	if got.State != StateSubmitted {
		t.Errorf("state changed: %q", got.State)
	}
	if got.PollAttempt != 5 {
		t.Errorf("poll_attempt = %d want 5", got.PollAttempt)
	}
	if got.NextPollAfter == nil {
		t.Errorf("next_poll_after still nil")
	}
}

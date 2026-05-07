package adapter

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────
// MockSyncAdapter
// ─────────────────────────────────────────────────────────────────────────

func TestMockSync_SubmitReturnsSyncResult(t *testing.T) {
	a := NewMockSyncAdapter()
	res, err := a.Submit(context.Background(), "model-x", Params{"prompt": "hi"}, "idem-1")
	if err != nil {
		t.Fatalf("Submit error: %v", err)
	}
	sync, ok := res.(SyncSubmit)
	if !ok {
		t.Fatalf("expected SyncSubmit, got %T", res)
	}
	if sync.Result == nil {
		t.Fatal("SyncSubmit.Result is nil")
	}
	if sync.Result.Modality != ModalityImage {
		t.Errorf("default modality = %q, want %q", sync.Result.Modality, ModalityImage)
	}
	if len(sync.Result.Outputs) != 1 {
		t.Fatalf("outputs len = %d, want 1", len(sync.Result.Outputs))
	}
	out := sync.Result.Outputs[0]
	if !strings.HasPrefix(out.URL, "https://cdn.modelhub.local/") {
		t.Errorf("URL %q is not CDN-shaped (AP-19)", out.URL)
	}
	if a.SubmitCount.Load() != 1 {
		t.Errorf("SubmitCount = %d, want 1", a.SubmitCount.Load())
	}
}

func TestMockSync_RejectsEmptyIdempotencyKey(t *testing.T) {
	a := NewMockSyncAdapter()
	_, err := a.Submit(context.Background(), "m", Params{}, "")
	if err == nil {
		t.Fatal("expected error for empty idempotency key (AP-12)")
	}
	if !errors.Is(err, ErrInvalidParams) {
		t.Errorf("err = %v; want ErrInvalidParams", err)
	}
}

func TestMockSync_ForceErrorClass(t *testing.T) {
	a := &MockSyncAdapter{ForceSubmitError: ErrClassRateLimit}
	_, err := a.Submit(context.Background(), "m", Params{}, "k")
	if err == nil {
		t.Fatal("expected error")
	}
	cls, ok := MockErrorClass(err)
	if !ok {
		t.Fatalf("MockErrorClass returned false on %v", err)
	}
	if cls != ErrClassRateLimit {
		t.Errorf("class = %q, want %q", cls, ErrClassRateLimit)
	}
}

func TestMockSync_PollAndCancelUnsupported(t *testing.T) {
	a := NewMockSyncAdapter()
	if _, err := a.Poll(context.Background(), "m", "ref"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Poll err = %v, want ErrUnsupported", err)
	}
	if err := a.Cancel(context.Background(), "m", "ref"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Cancel err = %v, want ErrUnsupported", err)
	}
	if _, err := a.VerifyWebhook(http.Header{}, []byte("{}")); !errors.Is(err, ErrUnsupported) {
		t.Errorf("VerifyWebhook err = %v, want ErrUnsupported", err)
	}
}

func TestMockSync_EstimateCostAndCaps(t *testing.T) {
	a := NewMockSyncAdapter()
	cost, err := a.EstimateCost("m", Params{})
	if err != nil {
		t.Fatal(err)
	}
	if cost != 40_000 {
		t.Errorf("default cost = %d, want 40000", cost)
	}
	caps := a.Capabilities("m")
	if caps.SupportsCancel || caps.SupportsWebhook {
		t.Errorf("default sync caps should be all-false, got %+v", caps)
	}
	custom := &MockSyncAdapter{
		FixedCost: 12345,
		Caps:      &ProviderCaps{SupportsCancel: true},
	}
	if c, _ := custom.EstimateCost("m", Params{}); c != 12345 {
		t.Errorf("FixedCost ignored: %d", c)
	}
	if !custom.Capabilities("m").SupportsCancel {
		t.Error("Caps override ignored")
	}
}

func TestMockSync_NormalizeResult(t *testing.T) {
	a := NewMockSyncAdapter()
	r, err := a.NormalizeResult("m", []byte(`{"ignored":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if r == nil || len(r.Outputs) != 1 {
		t.Fatalf("bad result: %+v", r)
	}
}

func TestMockSync_KeyConstant(t *testing.T) {
	if NewMockSyncAdapter().Key() != "mock-sync" {
		t.Fatal("Key drifted from mock-sync")
	}
}

func TestMockSync_SubmitDelayObservedAndCancellable(t *testing.T) {
	a := &MockSyncAdapter{SubmitDelay: 50 * time.Millisecond}
	start := time.Now()
	_, err := a.Submit(context.Background(), "m", Params{}, "k")
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Errorf("delay not observed: %v", elapsed)
	}
	// Now cancel via context.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = a.Submit(ctx, "m", Params{}, "k2")
	if err == nil {
		t.Fatal("expected ctx err")
	}
}

// ─────────────────────────────────────────────────────────────────────────
// MockAsyncAdapter
// ─────────────────────────────────────────────────────────────────────────

func TestMockAsync_SubmitReturnsAsyncSubmit(t *testing.T) {
	a := NewMockAsyncAdapter()
	res, err := a.Submit(context.Background(), "m", Params{"prompt": "x"}, "idem-1")
	if err != nil {
		t.Fatal(err)
	}
	asyncRes, ok := res.(AsyncSubmit)
	if !ok {
		t.Fatalf("expected AsyncSubmit, got %T", res)
	}
	if asyncRes.UpstreamRef == "" {
		t.Fatal("empty UpstreamRef")
	}
}

func TestMockAsync_PollCyclesPendingRunningSucceeded(t *testing.T) {
	a := NewMockAsyncAdapter() // default 3 steps
	res, err := a.Submit(context.Background(), "m", Params{}, "k")
	if err != nil {
		t.Fatal(err)
	}
	ref := res.(AsyncSubmit).UpstreamRef

	wantSequence := []PollStatus{PollPending, PollRunning, PollSucceeded}
	for i, want := range wantSequence {
		pr, err := a.Poll(context.Background(), "m", ref)
		if err != nil {
			t.Fatalf("poll %d: %v", i, err)
		}
		if pr.Status != want {
			t.Errorf("poll %d: status = %q, want %q", i, pr.Status, want)
		}
	}
	// Beyond N — stays succeeded.
	pr, _ := a.Poll(context.Background(), "m", ref)
	if pr.Status != PollSucceeded {
		t.Errorf("after success, status = %q", pr.Status)
	}
	if pr.Result == nil {
		t.Fatal("succeeded poll missing Result")
	}
}

func TestMockAsync_PollDoesNotSleep_AP3(t *testing.T) {
	// AP-3 guard: 100 polls under 50ms.
	a := NewMockAsyncAdapter()
	res, _ := a.Submit(context.Background(), "m", Params{}, "k")
	ref := res.(AsyncSubmit).UpstreamRef
	start := time.Now()
	for i := 0; i < 100; i++ {
		_, _ = a.Poll(context.Background(), "m", ref)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("AP-3 violation: 100 polls took %v (>50ms)", elapsed)
	}
}

func TestMockAsync_ProgressCurve(t *testing.T) {
	a := &MockAsyncAdapter{
		PollStepsToSucceed: 3,
		ProgressCurve:      []float32{0.1, 0.5, 1.0},
	}
	res, _ := a.Submit(context.Background(), "m", Params{}, "k")
	ref := res.(AsyncSubmit).UpstreamRef
	for i, want := range []float32{0.1, 0.5, 1.0} {
		pr, _ := a.Poll(context.Background(), "m", ref)
		if pr.Progress == nil {
			t.Fatalf("poll %d: nil Progress", i)
		}
		if *pr.Progress != want {
			t.Errorf("poll %d: progress = %v, want %v", i, *pr.Progress, want)
		}
	}
}

func TestMockAsync_CancelMarksRefAndPollFails(t *testing.T) {
	a := NewMockAsyncAdapter()
	res, _ := a.Submit(context.Background(), "m", Params{}, "k")
	ref := res.(AsyncSubmit).UpstreamRef
	if err := a.Cancel(context.Background(), "m", ref); err != nil {
		t.Fatal(err)
	}
	pr, _ := a.Poll(context.Background(), "m", ref)
	if pr.Status != PollFailed {
		t.Errorf("post-cancel status = %q, want failed", pr.Status)
	}
	if pr.Error == nil {
		t.Fatal("missing PollError")
	}
}

func TestMockAsync_CancelUnsupportedWhenCapsOverride(t *testing.T) {
	a := &MockAsyncAdapter{Caps: &ProviderCaps{}} // SupportsCancel=false
	err := a.Cancel(context.Background(), "m", "ref")
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("Cancel err = %v, want ErrUnsupported", err)
	}
}

func TestMockAsync_ForcePollErrorOnFinalStep(t *testing.T) {
	a := &MockAsyncAdapter{
		PollStepsToSucceed: 2,
		ForcePollError:     ErrClassContentPolicy,
	}
	res, _ := a.Submit(context.Background(), "m", Params{}, "k")
	ref := res.(AsyncSubmit).UpstreamRef
	// First poll → pending.
	pr, _ := a.Poll(context.Background(), "m", ref)
	if pr.Status != PollPending {
		t.Fatalf("first poll status = %q", pr.Status)
	}
	// Final poll → failed.
	pr, _ = a.Poll(context.Background(), "m", ref)
	if pr.Status != PollFailed {
		t.Fatalf("final poll status = %q", pr.Status)
	}
	if pr.Error.Class != ErrClassContentPolicy {
		t.Errorf("error class = %q", pr.Error.Class)
	}
}

func TestMockAsync_ConcurrentPollsForDifferentRefsAreIndependent(t *testing.T) {
	a := &MockAsyncAdapter{PollStepsToSucceed: 5}
	const n = 16
	refs := make([]UpstreamRef, n)
	for i := 0; i < n; i++ {
		res, err := a.Submit(context.Background(), "m", Params{}, IdempotencyKey(string(rune('a'+i))))
		if err != nil {
			t.Fatal(err)
		}
		refs[i] = res.(AsyncSubmit).UpstreamRef
	}
	var wg sync.WaitGroup
	for _, ref := range refs {
		ref := ref
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				_, _ = a.Poll(context.Background(), "m", ref)
			}
		}()
	}
	wg.Wait()
	if got := a.PollCount.Load(); got != int64(n*5) {
		t.Errorf("PollCount = %d, want %d", got, n*5)
	}
}

func TestMockAsync_RejectsEmptyIdempotencyKey(t *testing.T) {
	a := NewMockAsyncAdapter()
	_, err := a.Submit(context.Background(), "m", Params{}, "")
	if err == nil {
		t.Fatal("expected error for empty key")
	}
}

func TestMockAsync_VerifyWebhookUnsupportedByDefault(t *testing.T) {
	a := NewMockAsyncAdapter()
	_, err := a.VerifyWebhook(http.Header{}, []byte(`{"ref":"r"}`))
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("err = %v, want ErrUnsupported", err)
	}
}

func TestMockAsync_VerifyWebhookSucceeded(t *testing.T) {
	secret := []byte("topsecret")
	a := &MockAsyncAdapter{
		WebhookSupported: true,
		WebhookSecret:    secret,
	}
	body := []byte(`{"ref":"r-1","status":"succeeded"}`)
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	sig := hex.EncodeToString(mac.Sum(nil))
	hdr := http.Header{}
	hdr.Set("X-Mock-Signature", sig)
	v, err := a.VerifyWebhook(hdr, body)
	if err != nil {
		t.Fatal(err)
	}
	if v.UpstreamRef != "r-1" {
		t.Errorf("ref = %q", v.UpstreamRef)
	}
	if v.Result.Status != PollSucceeded {
		t.Errorf("status = %q", v.Result.Status)
	}
}

func TestMockAsync_VerifyWebhookFailedAndBadSig(t *testing.T) {
	secret := []byte("k")
	a := &MockAsyncAdapter{WebhookSupported: true, WebhookSecret: secret}
	body := []byte(`{"ref":"r","status":"failed","error_class":"upstream"}`)
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	good := hex.EncodeToString(mac.Sum(nil))
	hdr := http.Header{}
	hdr.Set("X-Mock-Signature", good)
	v, err := a.VerifyWebhook(hdr, body)
	if err != nil {
		t.Fatal(err)
	}
	if v.Result.Status != PollFailed || v.Result.Error.Class != ErrClassUpstream {
		t.Errorf("got %+v", v.Result)
	}

	// Bad sig → rejected.
	hdr.Set("X-Mock-Signature", strings.Repeat("00", 32))
	if _, err := a.VerifyWebhook(hdr, body); err == nil {
		t.Fatal("expected sig mismatch error")
	}

	// Missing header.
	if _, err := a.VerifyWebhook(http.Header{}, body); err == nil {
		t.Fatal("expected missing header error")
	}

	// Invalid hex.
	hdr.Set("X-Mock-Signature", "ZZZZ")
	if _, err := a.VerifyWebhook(hdr, body); err == nil {
		t.Fatal("expected hex decode error")
	}
}

func TestMockAsync_NormalizeResult(t *testing.T) {
	a := NewMockAsyncAdapter()
	r, err := a.NormalizeResult("m", []byte(`{"ref":"foo"}`))
	if err != nil {
		t.Fatal(err)
	}
	if r == nil || len(r.Outputs) != 1 {
		t.Fatal("bad result")
	}
	// Empty raw is tolerated.
	r, err = a.NormalizeResult("m", nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Outputs[0].URL == "" {
		t.Fatal("empty URL")
	}
}

func TestMockAsync_KeyConstant(t *testing.T) {
	if NewMockAsyncAdapter().Key() != "mock-async" {
		t.Fatal("Key drifted")
	}
}

func TestMockAsync_EstimateCostDefaultAndOverride(t *testing.T) {
	if c, _ := NewMockAsyncAdapter().EstimateCost("m", Params{}); c != 60_000 {
		t.Errorf("default async cost = %d, want 60_000", c)
	}
	custom := &MockAsyncAdapter{FixedCost: 99_999}
	if c, _ := custom.EstimateCost("m", Params{}); c != 99_999 {
		t.Errorf("FixedCost override async = %d", c)
	}
}

func TestMockAsync_SubmitContextCancel(t *testing.T) {
	a := NewMockAsyncAdapter()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := a.Submit(ctx, "m", Params{}, "k")
	if err == nil {
		t.Fatal("expected ctx err")
	}
}

func TestMockAsync_PollContextCancel(t *testing.T) {
	a := NewMockAsyncAdapter()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := a.Poll(ctx, "m", "ref")
	if err == nil {
		t.Fatal("expected ctx err")
	}
}

func TestMockAsync_CancelContextErr(t *testing.T) {
	a := NewMockAsyncAdapter()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.Cancel(ctx, "m", "ref"); err == nil {
		t.Fatal("expected ctx err")
	}
}

func TestMockAsync_ProgressCurveClamping(t *testing.T) {
	a := &MockAsyncAdapter{
		PollStepsToSucceed: 4,
		ProgressCurve:      []float32{-0.5, 0.5, 2.0},
	}
	res, _ := a.Submit(context.Background(), "m", Params{}, "k")
	ref := res.(AsyncSubmit).UpstreamRef
	got := []float32{}
	for i := 0; i < 4; i++ {
		pr, _ := a.Poll(context.Background(), "m", ref)
		if pr.Progress != nil {
			got = append(got, *pr.Progress)
		}
	}
	wantPrefix := []float32{0, 0.5, 1, 1}
	for i, w := range wantPrefix {
		if got[i] != w {
			t.Errorf("progress[%d] = %v, want %v", i, got[i], w)
		}
	}
}

func TestMockAsync_VerifyWebhookBadJSON(t *testing.T) {
	secret := []byte("k")
	a := &MockAsyncAdapter{WebhookSupported: true, WebhookSecret: secret}
	body := []byte(`not-json`)
	mac := hmacOf(secret, body)
	hdr := http.Header{}
	hdr.Set("X-Mock-Signature", mac)
	if _, err := a.VerifyWebhook(hdr, body); err == nil {
		t.Fatal("expected JSON parse error")
	}
}

func TestMockAsync_VerifyWebhookMissingRef(t *testing.T) {
	secret := []byte("k")
	a := &MockAsyncAdapter{WebhookSupported: true, WebhookSecret: secret}
	body := []byte(`{"status":"succeeded"}`)
	hdr := http.Header{}
	hdr.Set("X-Mock-Signature", hmacOf(secret, body))
	if _, err := a.VerifyWebhook(hdr, body); err == nil {
		t.Fatal("expected missing-ref error")
	}
}

func TestMockAsync_VerifyWebhookUnknownStatus(t *testing.T) {
	secret := []byte("k")
	a := &MockAsyncAdapter{WebhookSupported: true, WebhookSecret: secret}
	body := []byte(`{"ref":"r","status":"weird"}`)
	hdr := http.Header{}
	hdr.Set("X-Mock-Signature", hmacOf(secret, body))
	if _, err := a.VerifyWebhook(hdr, body); err == nil {
		t.Fatal("expected unknown-status error")
	}
}

func TestMockAsync_VerifyWebhookFailedDefaultClass(t *testing.T) {
	secret := []byte("k")
	a := &MockAsyncAdapter{WebhookSupported: true, WebhookSecret: secret}
	body := []byte(`{"ref":"r","status":"failed"}`)
	hdr := http.Header{}
	hdr.Set("X-Mock-Signature", hmacOf(secret, body))
	v, err := a.VerifyWebhook(hdr, body)
	if err != nil {
		t.Fatal(err)
	}
	if v.Result.Error.Class != ErrClassUnknown {
		t.Errorf("default class = %q, want %q", v.Result.Error.Class, ErrClassUnknown)
	}
}

func hmacOf(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// Sync-side coverage gap — exercise Submit context cancellation without delay,
// MockErrorClass on non-mock errors, and AcceptedAt on both result variants.
func TestMockSync_AcceptedAtPopulated(t *testing.T) {
	a := NewMockSyncAdapter()
	res, err := a.Submit(context.Background(), "m", Params{}, "k")
	if err != nil {
		t.Fatal(err)
	}
	if res.AcceptedAt().IsZero() {
		t.Error("SyncSubmit AcceptedAt is zero")
	}
}

func TestMockAsync_AcceptedAtPopulated(t *testing.T) {
	a := NewMockAsyncAdapter()
	res, err := a.Submit(context.Background(), "m", Params{}, "k")
	if err != nil {
		t.Fatal(err)
	}
	if res.AcceptedAt().IsZero() {
		t.Error("AsyncSubmit AcceptedAt is zero")
	}
}

func TestMockErrorClass_NonMockError(t *testing.T) {
	if _, ok := MockErrorClass(errors.New("plain error")); ok {
		t.Error("MockErrorClass returned ok=true for plain error")
	}
}

func TestMockSync_ContextCancelMidDelay(t *testing.T) {
	a := &MockSyncAdapter{SubmitDelay: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := a.Submit(ctx, "m", Params{}, "k")
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("did not honor cancel: %v", elapsed)
	}
}

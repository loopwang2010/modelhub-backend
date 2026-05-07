// AssetWorker — S9.5 background process that hosts upstream assets on
// our CDN. Subscribes to events.OutputAvailable from the EventBus; for
// each event, downloads the upstream URL and re-uploads under a stable
// per-task object key. On success: stamps task.output_url and emits
// AssetHosted. On terminal failure: emits AssetLost.
//
// AP-19 (no upstream URL exposure): the public API envelope reads
// task.output_url. The asset worker is the ONLY writer of that column.
// Until a task has a CDN URL, the envelope reports the task as still
// queued/running — clients never see the upstream URL.
//
// AP-13 (no full-payload buffering): downloads stream into Storage.Put
// directly via io.Copy. The reader hands chunks to the writer; nothing
// holds the full byte array in memory.
//
// Retry policy:
//
//	attempt 1 → immediately
//	attempt 2 → +1s + jitter
//	attempt 3 → +4s + jitter
//	attempt 4 → +9s + jitter   (DEFAULT MaxRetries=3 means up to 3 retries
//	                            after the first attempt; total 4 tries)
//
// After MaxRetries+1 failed attempts, emit AssetLost. ErrPermanent
// downloader errors short-circuit retries and emit AssetLost immediately.
//
// BFL-specific guard: BFL upstream URLs expire ~10 min after Submit; if
// the asset worker is slow and gets a 404 / 410 inside that window we
// classify those as ErrPermanent (the URL is genuinely dead). The
// downloader handles status mapping; the worker has no provider-specific
// awareness — that's a deliberate provider-agnostic invariant.

package asset

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/internal/events"
)

// DefaultMaxRetries is the per-event retry budget after the first attempt.
// Total attempts = MaxRetries + 1.
const DefaultMaxRetries = 3

// DefaultRetryBaseDelay is the base unit for the exponential backoff.
// Delay schedule: base * (attempt^2). With base=1s: 1s, 4s, 9s.
const DefaultRetryBaseDelay = time.Second

// AssetWorker subscribes to OutputAvailable events and re-hosts each one.
//
// Construction: NewAssetWorker(...). Start: w.Start(ctx) — subscribes and
// returns a Close func. Calls to Close() unsubscribe and wait for in-
// flight handlers to drain.
type AssetWorker struct {
	Bus        events.EventBus
	Downloader Downloader
	Storage    Storage
	URLWriter  TaskOutputURLWriter

	// MaxRetries: total = MaxRetries + 1 attempts. 0 → use DefaultMaxRetries.
	MaxRetries int

	// RetryBaseDelay: base for exponential backoff. 0 → DefaultRetryBaseDelay.
	RetryBaseDelay time.Duration

	// Now is the clock; tests override. Default time.Now.
	Now func() time.Time

	// HandlerTimeout caps the entire (download + upload + write + emit)
	// per-event flow. 0 → 5 * DefaultDownloadTimeout (5 minutes — enough
	// for a 4-attempt sequence with backoff).
	HandlerTimeout time.Duration

	// inflight tracks pending handler goroutines so Close() can drain.
	inflight sync.WaitGroup

	// rngMu + rng make jitter testable + race-free.
	rngMu sync.Mutex
	rng   *rand.Rand
}

// NewAssetWorker constructs an AssetWorker with required dependencies.
// All four dependencies (bus, downloader, storage, urlWriter) are
// required and panic on nil — these are programmer errors, not runtime
// conditions.
func NewAssetWorker(bus events.EventBus, dl Downloader, st Storage, ww TaskOutputURLWriter) *AssetWorker {
	if bus == nil {
		panic("asset: NewAssetWorker requires non-nil EventBus")
	}
	if dl == nil {
		panic("asset: NewAssetWorker requires non-nil Downloader")
	}
	if st == nil {
		panic("asset: NewAssetWorker requires non-nil Storage")
	}
	if ww == nil {
		panic("asset: NewAssetWorker requires non-nil TaskOutputURLWriter")
	}
	return &AssetWorker{
		Bus:            bus,
		Downloader:     dl,
		Storage:        st,
		URLWriter:      ww,
		MaxRetries:     DefaultMaxRetries,
		RetryBaseDelay: DefaultRetryBaseDelay,
		Now:            time.Now,
		HandlerTimeout: 5 * DefaultDownloadTimeout,
		rng:            rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// Start subscribes the worker to the event bus. Returns a Close function
// that unsubscribes and waits for in-flight handlers to drain.
//
// The supplied ctx is the worker's lifetime ctx — when it's done, no
// new events are accepted (best-effort), but in-flight events finish
// using the per-handler ctx to honor their own deadlines.
func (w *AssetWorker) Start(ctx context.Context) func() {
	unsub := w.Bus.Subscribe(func(ev events.Event) {
		oa, ok := ev.(events.OutputAvailable)
		if !ok {
			return
		}
		// Filter empty-URL events (base64 inline assets). See open
		// question §2 — out of scope for this iteration.
		if oa.UpstreamURL == "" {
			w.emitAssetLost(string(oa.GetTaskID()), "", "inline asset (empty URL) not yet supported by AssetWorker — see S9.5 open question §2")
			return
		}
		w.inflight.Add(1)
		go func() {
			defer w.inflight.Done()
			w.handle(ctx, oa)
		}()
	})
	return func() {
		unsub()
		w.inflight.Wait()
	}
}

// HandleSync processes one event in the caller's goroutine. Test entry
// point — production goes through Start.
func (w *AssetWorker) HandleSync(ctx context.Context, ev events.OutputAvailable) {
	w.handle(ctx, ev)
}

// handle is the per-event work loop. Wraps the entire flow in a single
// HandlerTimeout context.
func (w *AssetWorker) handle(parent context.Context, ev events.OutputAvailable) {
	ctx, cancel := context.WithTimeout(parent, w.HandlerTimeout)
	defer cancel()

	taskID := string(ev.GetTaskID())
	maxAttempts := w.MaxRetries + 1
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			lastErr = fmt.Errorf("ctx done before attempt %d: %w", attempt, err)
			break
		}
		put, err := w.tryOnce(ctx, ev)
		if err == nil {
			// Success — stamp the URL onto the task row.
			if writeErr := w.URLWriter.SetOutputURL(ctx, taskID, put.URL); writeErr != nil {
				// Treat as transient — the bytes are uploaded but the row
				// update failed. Let the worker retry once more so the task
				// row is consistent with storage; if that still fails, give
				// up and emit AssetLost.
				lastErr = fmt.Errorf("write output_url: %w", writeErr)
				if attempt < maxAttempts {
					w.sleepBackoff(ctx, attempt)
					continue
				}
				w.emitAssetLost(taskID, ev.UpstreamURL, lastErr.Error())
				return
			}
			w.emitAssetHosted(taskID, put.URL, put.SizeBytes)
			return
		}
		lastErr = err
		// ErrPermanent: do not retry.
		var perm *ErrPermanent
		if errors.As(err, &perm) {
			break
		}
		// Transient or unclassified: retry until budget exhausted.
		if attempt < maxAttempts {
			w.sleepBackoff(ctx, attempt)
		}
	}
	reason := "unknown failure"
	if lastErr != nil {
		reason = lastErr.Error()
	}
	w.emitAssetLost(taskID, ev.UpstreamURL, reason)
}

// tryOnce performs one (download → upload) attempt. Returns the put
// result on success.
//
// Streaming guarantee (AP-13): the download body is io.Copy'd directly
// into Storage.Put without intermediate buffering.
func (w *AssetWorker) tryOnce(ctx context.Context, ev events.OutputAvailable) (*PutResult, error) {
	dl, err := w.Downloader.Download(ctx, ev.UpstreamURL)
	if err != nil {
		return nil, err
	}
	defer func() { _ = dl.Body.Close() }()

	mime := pickMime(ev.MimeType, dl.ContentType)
	key := BuildObjectKey(string(ev.GetTaskID()), ev.UpstreamURL, mime)
	put, err := w.Storage.Put(ctx, key, mime, dl.Body)
	if err != nil {
		return nil, &ErrTransient{Err: fmt.Errorf("storage put: %w", err)}
	}
	return put, nil
}

// sleepBackoff sleeps for an exponentially-growing delay before the next
// attempt. Jitter is +/- 25% of the base delay.
func (w *AssetWorker) sleepBackoff(ctx context.Context, attempt int) {
	base := w.RetryBaseDelay
	if base <= 0 {
		base = DefaultRetryBaseDelay
	}
	// attempt^2 backoff: 1, 4, 9, 16, ...
	delay := base * time.Duration(attempt*attempt)
	jitter := w.jitterFraction()
	delay = delay + time.Duration(float64(base)*jitter)
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}
}

// jitterFraction returns a random number in [-0.25, +0.25].
func (w *AssetWorker) jitterFraction() float64 {
	w.rngMu.Lock()
	defer w.rngMu.Unlock()
	return (w.rng.Float64() - 0.5) / 2
}

// emitAssetHosted publishes a single AssetHosted event. Failure to
// publish is logged-and-ignored; subscribers are best-effort.
func (w *AssetWorker) emitAssetHosted(taskID, cdnURL string, size int64) {
	_ = w.Bus.Publish(events.MakeAssetHosted(
		events.NewBaseEvent(events.TaskID(taskID)), cdnURL, size,
	))
}

// emitAssetLost publishes a single AssetLost event.
func (w *AssetWorker) emitAssetLost(taskID, upstreamURL, reason string) {
	_ = w.Bus.Publish(events.MakeAssetLost(
		events.NewBaseEvent(events.TaskID(taskID)), upstreamURL, reason,
	))
}

// pickMime returns the upstream-asserted mime if present, else the HTTP
// Content-Type from the actual download. Empty string when neither is
// available (the caller still uploads — Storage.Put treats empty mime as
// application/octet-stream).
func pickMime(eventMime, downloadMime string) string {
	if eventMime != "" {
		return eventMime
	}
	if downloadMime != "" {
		// Strip parameters like "; charset=..." to keep the stored mime
		// minimal.
		if idx := strings.Index(downloadMime, ";"); idx >= 0 {
			return strings.TrimSpace(downloadMime[:idx])
		}
		return downloadMime
	}
	return ""
}

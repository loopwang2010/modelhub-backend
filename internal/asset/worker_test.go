package asset

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/internal/events"
)

// ─── shared test helpers ───

type recordedBus struct {
	bus     *events.MemoryBus
	mu      sync.Mutex
	emitted []events.Event
}

func newRecordedBus() *recordedBus {
	rb := &recordedBus{bus: events.NewMemoryBus()}
	rb.bus.Subscribe(func(ev events.Event) {
		rb.mu.Lock()
		defer rb.mu.Unlock()
		rb.emitted = append(rb.emitted, ev)
	})
	return rb
}

func (r *recordedBus) Subscribe(h events.Handler) events.Unsubscribe {
	return r.bus.Subscribe(h)
}
func (r *recordedBus) Publish(ev events.Event) error {
	return r.bus.Publish(ev)
}

func (r *recordedBus) byKind(kind string) []events.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]events.Event, 0)
	for _, e := range r.emitted {
		if e.Kind() == kind {
			out = append(out, e)
		}
	}
	return out
}

// memWriter is an in-memory TaskOutputURLWriter for tests that don't
// need real DB.
type memWriter struct {
	mu    sync.Mutex
	urls  map[string]string
	fail  func(string) error
	calls atomic.Int64
}

func newMemWriter() *memWriter { return &memWriter{urls: map[string]string{}} }

func (m *memWriter) SetOutputURL(_ context.Context, taskID, cdnURL string) error {
	m.calls.Add(1)
	if m.fail != nil {
		if err := m.fail(taskID); err != nil {
			return err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.urls[taskID] = cdnURL
	return nil
}

func (m *memWriter) get(taskID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.urls[taskID]
}

// flakyDownloader fails the first N attempts then succeeds.
type flakyDownloader struct {
	failsRemaining atomic.Int64
	transient      bool
	bytes          []byte
}

func (f *flakyDownloader) Download(ctx context.Context, src string) (*DownloadResult, error) {
	if f.failsRemaining.Add(-1) >= 0 {
		err := fmt.Errorf("download: simulated failure")
		if f.transient {
			return nil, &ErrTransient{Err: err}
		}
		return nil, &ErrPermanent{Err: err}
	}
	return &DownloadResult{
		Body:        io.NopCloser(strings.NewReader(string(f.bytes))),
		ContentType: "image/png",
		SizeBytes:   int64(len(f.bytes)),
	}, nil
}

// ─── tests ───

func TestAssetWorker_HappyPath_HTTP(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("pngbytes"))
	}))
	defer srv.Close()

	bus := newRecordedBus()
	dl := NewSchemeDispatcher(NewHTTPDownloader(), nil)
	st := mustLocalStore(t)
	wr := newMemWriter()
	w := newTestAssetWorker(bus, dl, st, wr)

	ev := events.MakeOutputAvailable(
		events.NewBaseEvent(events.TaskID("gen_happy")),
		srv.URL+"/img.png",
		"image/png", 8,
	)
	w.HandleSync(context.Background(), ev)

	hosted := bus.byKind("asset_hosted")
	if len(hosted) != 1 {
		t.Fatalf("want 1 asset_hosted event, got %d", len(hosted))
	}
	ah := hosted[0].(events.AssetHosted)
	if !strings.HasPrefix(ah.CDNURL, "https://cdn.modelhub.local/outputs/gen_happy/") {
		t.Errorf("CDN URL: %q", ah.CDNURL)
	}
	if ah.SizeBytes != 8 {
		t.Errorf("size: %d", ah.SizeBytes)
	}
	if got := wr.get("gen_happy"); got != ah.CDNURL {
		t.Errorf("URL writer: want %q, got %q", ah.CDNURL, got)
	}
	if lost := bus.byKind("asset_lost"); len(lost) != 0 {
		t.Errorf("unexpected asset_lost: %d", len(lost))
	}
}

func TestAssetWorker_RetryOn5xxThenSucceed(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n <= 2 {
			http.Error(w, "boom", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	bus := newRecordedBus()
	dl := NewSchemeDispatcher(NewHTTPDownloader(), nil)
	st := mustLocalStore(t)
	wr := newMemWriter()
	w := newTestAssetWorker(bus, dl, st, wr)
	w.MaxRetries = 3
	w.RetryBaseDelay = 1 * time.Millisecond

	ev := events.MakeOutputAvailable(
		events.NewBaseEvent(events.TaskID("gen_retry")),
		srv.URL+"/x.png",
		"image/png", 0,
	)
	w.HandleSync(context.Background(), ev)

	hosted := bus.byKind("asset_hosted")
	if len(hosted) != 1 {
		t.Fatalf("want 1 asset_hosted, got %d", len(hosted))
	}
	if calls.Load() < 3 {
		t.Errorf("expected at least 3 attempts, got %d", calls.Load())
	}
}

func TestAssetWorker_AllRetriesFail_AssetLost(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer srv.Close()

	bus := newRecordedBus()
	dl := NewSchemeDispatcher(NewHTTPDownloader(), nil)
	st := mustLocalStore(t)
	wr := newMemWriter()
	w := newTestAssetWorker(bus, dl, st, wr)
	w.MaxRetries = 2
	w.RetryBaseDelay = 1 * time.Millisecond

	ev := events.MakeOutputAvailable(
		events.NewBaseEvent(events.TaskID("gen_lost")),
		srv.URL+"/x.png",
		"image/png", 0,
	)
	w.HandleSync(context.Background(), ev)

	if hosted := bus.byKind("asset_hosted"); len(hosted) != 0 {
		t.Errorf("unexpected asset_hosted: %d", len(hosted))
	}
	lost := bus.byKind("asset_lost")
	if len(lost) != 1 {
		t.Fatalf("want 1 asset_lost, got %d", len(lost))
	}
	al := lost[0].(events.AssetLost)
	if al.UpstreamURL != srv.URL+"/x.png" {
		t.Errorf("AssetLost.UpstreamURL: %q", al.UpstreamURL)
	}
}

func TestAssetWorker_PermanentError_NoRetry(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusGone)
	}))
	defer srv.Close()

	var attempts atomic.Int64
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		http.Error(w, "gone", http.StatusGone)
	}))
	defer srv2.Close()

	bus := newRecordedBus()
	dl := NewSchemeDispatcher(NewHTTPDownloader(), nil)
	st := mustLocalStore(t)
	wr := newMemWriter()
	w := newTestAssetWorker(bus, dl, st, wr)
	w.MaxRetries = 5
	w.RetryBaseDelay = 1 * time.Millisecond

	ev := events.MakeOutputAvailable(
		events.NewBaseEvent(events.TaskID("gen_perm")),
		srv2.URL+"/x.png",
		"image/png", 0,
	)
	w.HandleSync(context.Background(), ev)

	if attempts.Load() != 1 {
		t.Errorf("permanent error must not retry; attempts=%d", attempts.Load())
	}
	if lost := bus.byKind("asset_lost"); len(lost) != 1 {
		t.Errorf("want 1 asset_lost, got %d", len(lost))
	}
}

func TestAssetWorker_EmptyURL_EmitsAssetLost(t *testing.T) {
	t.Parallel()
	// Empty URL = inline base64 case. AssetWorker logs+emits AssetLost
	// per open question §2.

	bus := newRecordedBus()
	dl := NewSchemeDispatcher(NewHTTPDownloader(), nil)
	st := mustLocalStore(t)
	wr := newMemWriter()
	w := newTestAssetWorker(bus, dl, st, wr)

	stop := w.Start(context.Background())
	defer stop()

	_ = bus.Publish(events.MakeOutputAvailable(
		events.NewBaseEvent(events.TaskID("gen_inline")),
		"", "image/png", 1024,
	))

	// asynchronous — wait briefly.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(bus.byKind("asset_lost")) >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if lost := bus.byKind("asset_lost"); len(lost) != 1 {
		t.Errorf("want 1 asset_lost, got %d", len(lost))
	}
	if hosted := bus.byKind("asset_hosted"); len(hosted) != 0 {
		t.Errorf("unexpected asset_hosted: %d", len(hosted))
	}
}

func TestAssetWorker_GSDownload_StubbedSuccess(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/storage/v1/b/") {
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("mp4-content"))
	}))
	defer srv.Close()

	bus := newRecordedBus()
	gcs := NewGCSDownloader(&http.Client{Transport: &fakeAuthTransport{token: "t"}})
	gcs.Endpoint = srv.URL
	dl := NewSchemeDispatcher(NewHTTPDownloader(), gcs)
	st := mustLocalStore(t)
	wr := newMemWriter()
	w := newTestAssetWorker(bus, dl, st, wr)

	ev := events.MakeOutputAvailable(
		events.NewBaseEvent(events.TaskID("gen_gs")),
		"gs://my-bucket/path/to/video.mp4",
		"video/mp4", 0,
	)
	w.HandleSync(context.Background(), ev)

	hosted := bus.byKind("asset_hosted")
	if len(hosted) != 1 {
		t.Fatalf("want 1 asset_hosted, got %d", len(hosted))
	}
	ah := hosted[0].(events.AssetHosted)
	if !strings.HasSuffix(ah.CDNURL, ".mp4") {
		t.Errorf("CDN URL should end .mp4: %q", ah.CDNURL)
	}
	if got := wr.get("gen_gs"); got != ah.CDNURL {
		t.Errorf("URL writer: want %q, got %q", ah.CDNURL, got)
	}
}

func TestAssetWorker_URLWriterFails_RetriesThenLost(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	bus := newRecordedBus()
	dl := NewSchemeDispatcher(NewHTTPDownloader(), nil)
	st := mustLocalStore(t)
	wr := newMemWriter()
	wr.fail = func(string) error { return errors.New("db down") }
	w := newTestAssetWorker(bus, dl, st, wr)
	w.MaxRetries = 2
	w.RetryBaseDelay = 1 * time.Millisecond

	ev := events.MakeOutputAvailable(
		events.NewBaseEvent(events.TaskID("gen_dbfail")),
		srv.URL+"/x.png",
		"image/png", 0,
	)
	w.HandleSync(context.Background(), ev)

	// All write attempts fail → AssetLost.
	if lost := bus.byKind("asset_lost"); len(lost) != 1 {
		t.Errorf("want 1 asset_lost, got %d", len(lost))
	}
	if wr.calls.Load() < 1 {
		t.Errorf("URL writer not called")
	}
}

func TestAssetWorker_NilDeps_Panic(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		fn   func()
	}{
		{"nil bus", func() { _ = NewAssetWorker(nil, &stubDownloader{}, &stubStorage{}, newMemWriter()) }},
		{"nil downloader", func() { _ = NewAssetWorker(events.NewMemoryBus(), nil, &stubStorage{}, newMemWriter()) }},
		{"nil storage", func() { _ = NewAssetWorker(events.NewMemoryBus(), &stubDownloader{}, nil, newMemWriter()) }},
		{"nil writer", func() { _ = NewAssetWorker(events.NewMemoryBus(), &stubDownloader{}, &stubStorage{}, nil) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Error("expected panic")
				}
			}()
			tc.fn()
		})
	}
}

func TestAssetWorker_ConcurrentDownloads_NoCollision(t *testing.T) {
	// 50 concurrent OutputAvailable events for distinct tasks. Verifies
	// no race on bucket-key generation (each task has a unique URL/key).
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("p:" + r.URL.Path))
	}))
	defer srv.Close()

	bus := newRecordedBus()
	dl := NewSchemeDispatcher(NewHTTPDownloader(), nil)
	st := mustLocalStore(t)
	wr := newMemWriter()
	w := newTestAssetWorker(bus, dl, st, wr)
	w.HandlerTimeout = 30 * time.Second
	stop := w.Start(context.Background())
	defer stop()

	const N = 50
	for i := 0; i < N; i++ {
		_ = bus.Publish(events.MakeOutputAvailable(
			events.NewBaseEvent(events.TaskID(fmt.Sprintf("gen_%03d", i))),
			fmt.Sprintf("%s/img-%d.png", srv.URL, i),
			"image/png", 0,
		))
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(bus.byKind("asset_hosted"))+len(bus.byKind("asset_lost")) >= N {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	hosted := bus.byKind("asset_hosted")
	if len(hosted) != N {
		t.Errorf("want %d asset_hosted, got %d (lost=%d)", N, len(hosted), len(bus.byKind("asset_lost")))
	}
	// Verify all task IDs got distinct CDN URLs.
	seen := make(map[string]int)
	for _, e := range hosted {
		ah := e.(events.AssetHosted)
		seen[ah.CDNURL]++
	}
	for url, count := range seen {
		if count != 1 {
			t.Errorf("collision on URL %q (count=%d)", url, count)
		}
	}
}

func TestAssetWorker_FlakyDownloaderRecovers(t *testing.T) {
	t.Parallel()
	// 2 transient failures then success — within the retry budget.
	bus := newRecordedBus()
	fl := &flakyDownloader{transient: true, bytes: []byte("ok")}
	fl.failsRemaining.Store(2)
	dl := NewSchemeDispatcher(fl, nil)
	st := mustLocalStore(t)
	wr := newMemWriter()
	w := newTestAssetWorker(bus, dl, st, wr)
	w.MaxRetries = 3
	w.RetryBaseDelay = 1 * time.Millisecond

	ev := events.MakeOutputAvailable(
		events.NewBaseEvent(events.TaskID("gen_flk")),
		"https://upstream/x.png",
		"image/png", 2,
	)
	w.HandleSync(context.Background(), ev)
	if hosted := bus.byKind("asset_hosted"); len(hosted) != 1 {
		t.Errorf("want 1 asset_hosted, got %d", len(hosted))
	}
}

func TestAssetWorker_FlakyPermanent_AssetLostImmediately(t *testing.T) {
	t.Parallel()
	bus := newRecordedBus()
	fl := &flakyDownloader{transient: false, bytes: []byte("never seen")}
	fl.failsRemaining.Store(100)
	dl := NewSchemeDispatcher(fl, nil)
	st := mustLocalStore(t)
	wr := newMemWriter()
	w := newTestAssetWorker(bus, dl, st, wr)
	w.MaxRetries = 5
	w.RetryBaseDelay = 1 * time.Millisecond

	ev := events.MakeOutputAvailable(
		events.NewBaseEvent(events.TaskID("gen_pflk")),
		"https://upstream/x.png",
		"image/png", 0,
	)
	w.HandleSync(context.Background(), ev)
	// permanent error: should fail on first attempt → AssetLost.
	if lost := bus.byKind("asset_lost"); len(lost) != 1 {
		t.Errorf("want 1 asset_lost, got %d", len(lost))
	}
}

func TestAssetWorker_StartCloseDrain(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Slight delay so close has work in flight.
		time.Sleep(10 * time.Millisecond)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	bus := newRecordedBus()
	dl := NewSchemeDispatcher(NewHTTPDownloader(), nil)
	st := mustLocalStore(t)
	wr := newMemWriter()
	w := newTestAssetWorker(bus, dl, st, wr)
	stop := w.Start(context.Background())

	for i := 0; i < 5; i++ {
		_ = bus.Publish(events.MakeOutputAvailable(
			events.NewBaseEvent(events.TaskID(fmt.Sprintf("gen_drain_%d", i))),
			fmt.Sprintf("%s/x-%d.png", srv.URL, i),
			"image/png", 0,
		))
	}
	// stop() must wait for in-flight handlers.
	stop()
	hosted := bus.byKind("asset_hosted")
	if len(hosted) != 5 {
		t.Errorf("after drain: want 5 hosted, got %d", len(hosted))
	}
}

func TestAssetWorker_NonOutputAvailableEvent_Ignored(t *testing.T) {
	t.Parallel()

	bus := newRecordedBus()
	dl := NewSchemeDispatcher(NewHTTPDownloader(), nil)
	st := mustLocalStore(t)
	wr := newMemWriter()
	w := newTestAssetWorker(bus, dl, st, wr)
	stop := w.Start(context.Background())
	defer stop()

	// Publish unrelated events — worker must not react.
	_ = bus.Publish(events.MakeTaskSucceeded(events.NewBaseEvent("gen_x"), 100))
	_ = bus.Publish(events.MakeTaskFailed(events.NewBaseEvent("gen_y"), "auth", "no key"))

	time.Sleep(50 * time.Millisecond)
	if hosted := bus.byKind("asset_hosted"); len(hosted) != 0 {
		t.Errorf("unexpected asset_hosted: %d", len(hosted))
	}
	if lost := bus.byKind("asset_lost"); len(lost) != 0 {
		t.Errorf("unexpected asset_lost: %d", len(lost))
	}
}

func TestPickMime(t *testing.T) {
	t.Parallel()
	cases := []struct {
		event, dl, want string
	}{
		{"image/png", "video/mp4", "image/png"},
		{"", "video/mp4", "video/mp4"},
		{"", "image/png; charset=binary", "image/png"},
		{"", "", ""},
	}
	for _, tc := range cases {
		if got := pickMime(tc.event, tc.dl); got != tc.want {
			t.Errorf("pickMime(%q,%q)=%q want %q", tc.event, tc.dl, got, tc.want)
		}
	}
}

// ─── helpers ───

// newTestAssetWorker constructs an AssetWorker tuned for fast tests.
func newTestAssetWorker(bus events.EventBus, dl Downloader, st Storage, wr TaskOutputURLWriter) *AssetWorker {
	w := NewAssetWorker(bus, dl, st, wr)
	w.MaxRetries = 1
	w.RetryBaseDelay = 1 * time.Millisecond
	w.HandlerTimeout = 5 * time.Second
	return w
}

// stubStorage records every Put.
type stubStorage struct {
	puts atomic.Int64
}

func (s *stubStorage) Put(ctx context.Context, key, ct string, body io.Reader) (*PutResult, error) {
	s.puts.Add(1)
	// Drain body so caller sees stream completion.
	_, _ = io.Copy(io.Discard, body)
	return &PutResult{Key: key, URL: "https://cdn.modelhub.local/" + key, SizeBytes: 0}, nil
}

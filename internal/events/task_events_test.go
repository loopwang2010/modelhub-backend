package events

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestEventVariants_KindAndIDAndAt asserts every variant returns its Kind,
// embeds TaskID, and threads through baseEvent.
func TestEventVariants_KindAndIDAndAt(t *testing.T) {
	at := NowUTC()
	tid := TaskID("gen_test")
	cases := []struct {
		name string
		ev   Event
		kind string
	}{
		{"held", TaskHeld{baseEvent: baseEvent{TaskID: tid, When: at}, HeldAmount: 1, Model: "m"}, "task_held"},
		{"submitted", TaskSubmitted{baseEvent: baseEvent{TaskID: tid, When: at}}, "task_submitted"},
		{"running", TaskRunning{baseEvent: baseEvent{TaskID: tid, When: at}}, "task_running"},
		{"succeeded", TaskSucceeded{baseEvent: baseEvent{TaskID: tid, When: at}, ActualCost: 1}, "task_succeeded"},
		{"failed", TaskFailed{baseEvent: baseEvent{TaskID: tid, When: at}, ErrorClass: "auth"}, "task_failed"},
		{"timed_out", TaskTimedOut{baseEvent: baseEvent{TaskID: tid, When: at}}, "task_timed_out"},
		{"cancelled", TaskCancelled{baseEvent: baseEvent{TaskID: tid, When: at}, By: "user"}, "task_cancelled"},
		{"output", OutputAvailable{baseEvent: baseEvent{TaskID: tid, When: at}, UpstreamURL: "u"}, "output_available"},
		{"hosted", AssetHosted{baseEvent: baseEvent{TaskID: tid, When: at}, CDNURL: "c"}, "asset_hosted"},
		{"lost", AssetLost{baseEvent: baseEvent{TaskID: tid, When: at}, Reason: "x"}, "asset_lost"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.ev.GetTaskID() != tid {
				t.Errorf("GetTaskID = %q, want %q", tc.ev.GetTaskID(), tid)
			}
			if !tc.ev.At().Equal(at) {
				t.Errorf("At = %v, want %v", tc.ev.At(), at)
			}
			if tc.ev.Kind() != tc.kind {
				t.Errorf("Kind = %q, want %q", tc.ev.Kind(), tc.kind)
			}
		})
	}
}

func TestNewBase_PopulatesTaskIDAndUTC(t *testing.T) {
	b := NewBase("gen_x")
	if b.TaskID != "gen_x" {
		t.Errorf("TaskID = %q", b.TaskID)
	}
	if b.When.Location() != time.UTC {
		t.Errorf("When in non-UTC zone: %v", b.When.Location())
	}
}

// TestMemoryBus_DeliversToAllSubscribersOnce: blueprint exit criterion —
// "EventBus delivers TaskHeld/TaskSucceeded to two subscribers, exactly
// once each, in unit test."
func TestMemoryBus_DeliversToAllSubscribersOnce(t *testing.T) {
	b := NewMemoryBus()
	var aCount, bCount atomic.Int32
	b.Subscribe(func(e Event) { aCount.Add(1) })
	b.Subscribe(func(e Event) { bCount.Add(1) })

	if err := b.Publish(TaskHeld{baseEvent: NewBase("t1"), HeldAmount: 100}); err != nil {
		t.Fatal(err)
	}
	if err := b.Publish(TaskSucceeded{baseEvent: NewBase("t1"), ActualCost: 90}); err != nil {
		t.Fatal(err)
	}
	if aCount.Load() != 2 {
		t.Errorf("subscriber A got %d events; want 2", aCount.Load())
	}
	if bCount.Load() != 2 {
		t.Errorf("subscriber B got %d events; want 2", bCount.Load())
	}
}

func TestMemoryBus_UnsubscribeStopsDelivery(t *testing.T) {
	b := NewMemoryBus()
	var got atomic.Int32
	un := b.Subscribe(func(e Event) { got.Add(1) })
	b.Publish(TaskHeld{baseEvent: NewBase("t1")})
	un()
	b.Publish(TaskHeld{baseEvent: NewBase("t1")})
	if got.Load() != 1 {
		t.Errorf("got = %d, want 1", got.Load())
	}
	// Idempotent — second call must not panic.
	un()
}

func TestMemoryBus_PublishNilEventErrors(t *testing.T) {
	b := NewMemoryBus()
	if err := b.Publish(nil); err == nil {
		t.Fatal("expected error on Publish(nil)")
	}
}

func TestMemoryBus_SubscribeNilHandlerPanics(t *testing.T) {
	b := NewMemoryBus()
	defer func() {
		if recover() == nil {
			t.Error("expected panic on Subscribe(nil)")
		}
	}()
	b.Subscribe(nil)
}

func TestMemoryBus_CloseRejectsPublish(t *testing.T) {
	b := NewMemoryBus()
	b.Close()
	err := b.Publish(TaskHeld{baseEvent: NewBase("t1")})
	if err == nil {
		t.Fatal("expected ErrBusClosed")
	}
	if !errors.Is(err, ErrBusClosed) {
		t.Errorf("err = %v, want ErrBusClosed", err)
	}
}

func TestMemoryBus_Len(t *testing.T) {
	b := NewMemoryBus()
	if b.Len() != 0 {
		t.Errorf("empty Len = %d", b.Len())
	}
	un1 := b.Subscribe(func(e Event) {})
	un2 := b.Subscribe(func(e Event) {})
	if b.Len() != 2 {
		t.Errorf("Len after 2 = %d", b.Len())
	}
	un1()
	if b.Len() != 1 {
		t.Errorf("Len after 1 unsub = %d", b.Len())
	}
	un2()
	if b.Len() != 0 {
		t.Errorf("Len after 2 unsub = %d", b.Len())
	}
}

func TestMemoryBus_ConcurrentPublishAndSubscribe(t *testing.T) {
	b := NewMemoryBus()
	var got atomic.Int32
	const subs = 4
	for i := 0; i < subs; i++ {
		b.Subscribe(func(e Event) { got.Add(1) })
	}
	const events = 100
	var wg sync.WaitGroup
	const publishers = 4
	for p := 0; p < publishers; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < events; i++ {
				_ = b.Publish(TaskHeld{baseEvent: NewBase("t")})
			}
		}()
	}
	wg.Wait()
	want := int32(subs * publishers * events)
	if got.Load() != want {
		t.Errorf("delivered = %d, want %d", got.Load(), want)
	}
}

// TestMemoryBus_HandlerSubscribesDuringPublish: re-entrancy. A handler that
// subscribes a second handler during Publish must not deadlock.
func TestMemoryBus_HandlerSubscribesDuringPublish(t *testing.T) {
	b := NewMemoryBus()
	var second atomic.Int32
	b.Subscribe(func(e Event) {
		// Subscribe from inside a delivering handler. Snapshot semantics
		// mean this handler doesn't see the current event but does see
		// future ones.
		b.Subscribe(func(e Event) { second.Add(1) })
	})
	b.Publish(TaskHeld{baseEvent: NewBase("t1")})
	b.Publish(TaskSucceeded{baseEvent: NewBase("t1")})
	if got := second.Load(); got == 0 {
		t.Errorf("second handler delivered 0 times; expected ≥1")
	}
}

// TestSealed_EventInterfaceIsClosed: documentation guard. New variants must
// be added in this package; external types cannot satisfy Event because
// eventKind() is unexported.
//
// We can only assert this loosely: the concrete variants we have all
// satisfy Event. (An external attempt to implement Event would fail to
// compile, which is the point.)
func TestSealed_AllVariantsSatisfyEvent(t *testing.T) {
	var _ Event = TaskHeld{}
	var _ Event = TaskSubmitted{}
	var _ Event = TaskRunning{}
	var _ Event = TaskSucceeded{}
	var _ Event = TaskFailed{}
	var _ Event = TaskTimedOut{}
	var _ Event = TaskCancelled{}
	var _ Event = OutputAvailable{}
	var _ Event = AssetHosted{}
	var _ Event = AssetLost{}
}

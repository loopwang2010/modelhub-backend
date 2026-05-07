// Constructors for event variants.
//
// task_events.go (the locked S2.5 file) declares the variants but uses
// an unexported `baseEvent` field that cannot be initialized from
// outside the events package. S5 (and S6, S9.5 later) need to publish
// events without modifying the locked file.
//
// This file lives in the same package so it can name `baseEvent` and
// the unexported field. It is additive — task_events.go is untouched.
//
// Constructor naming: Make<Variant> rather than New<Variant> to keep the
// existing NewMemoryBus / NewBase pattern recognizable as the package's
// "primary constructor" set.

package events

// BaseEvent is the publicly nameable alias of baseEvent. External
// packages can request one via NewBaseEvent and pass it back into the
// constructors below.
type BaseEvent = baseEvent

// NewBaseEvent returns a baseEvent for taskID at NowUTC. Equivalent to
// the existing NewBase but with a name that doesn't collide.
func NewBaseEvent(taskID TaskID) BaseEvent {
	return baseEvent{TaskID: taskID, When: NowUTC()}
}

// MakeTaskHeld constructs a TaskHeld event.
func MakeTaskHeld(b BaseEvent, held CostUSD, model, idemKey string) TaskHeld {
	return TaskHeld{
		baseEvent:      b,
		HeldAmount:     held,
		Model:          model,
		IdempotencyKey: idemKey,
	}
}

// MakeTaskSubmitted constructs a TaskSubmitted event.
func MakeTaskSubmitted(b BaseEvent, provider, upstreamRef string) TaskSubmitted {
	return TaskSubmitted{
		baseEvent:   b,
		Provider:    provider,
		UpstreamRef: upstreamRef,
	}
}

// MakeTaskRunning constructs a TaskRunning event.
func MakeTaskRunning(b BaseEvent, progress *float32) TaskRunning {
	return TaskRunning{
		baseEvent: b,
		Progress:  progress,
	}
}

// MakeTaskSucceeded constructs a TaskSucceeded event.
func MakeTaskSucceeded(b BaseEvent, actual CostUSD) TaskSucceeded {
	return TaskSucceeded{
		baseEvent:  b,
		ActualCost: actual,
	}
}

// MakeTaskFailed constructs a TaskFailed event.
func MakeTaskFailed(b BaseEvent, class ErrorClass, message string) TaskFailed {
	return TaskFailed{
		baseEvent:  b,
		ErrorClass: class,
		Message:    message,
	}
}

// MakeTaskTimedOut constructs a TaskTimedOut event.
func MakeTaskTimedOut(b BaseEvent, prepaid CostUSD) TaskTimedOut {
	return TaskTimedOut{
		baseEvent:     b,
		PrepaidAmount: prepaid,
	}
}

// MakeTaskCancelled constructs a TaskCancelled event.
func MakeTaskCancelled(b BaseEvent, refund CostUSD, by string) TaskCancelled {
	return TaskCancelled{
		baseEvent:    b,
		RefundAmount: refund,
		By:           by,
	}
}

// MakeOutputAvailable constructs an OutputAvailable event.
func MakeOutputAvailable(b BaseEvent, upstreamURL, mimeType string, sizeBytes int64) OutputAvailable {
	return OutputAvailable{
		baseEvent:   b,
		UpstreamURL: upstreamURL,
		MimeType:    mimeType,
		SizeBytes:   sizeBytes,
	}
}

// MakeAssetHosted constructs an AssetHosted event.
func MakeAssetHosted(b BaseEvent, cdnURL string, sizeBytes int64) AssetHosted {
	return AssetHosted{
		baseEvent: b,
		CDNURL:    cdnURL,
		SizeBytes: sizeBytes,
	}
}

// MakeAssetLost constructs an AssetLost event.
func MakeAssetLost(b BaseEvent, upstreamURL, reason string) AssetLost {
	return AssetLost{
		baseEvent:   b,
		UpstreamURL: upstreamURL,
		Reason:      reason,
	}
}

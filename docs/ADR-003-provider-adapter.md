# ADR-003 — ProviderAdapter is the most consequential interface

**Status:** Accepted (2026-05-07)
**Sprint:** S2 (design / core-interfaces)
**Authors:** modelhub team

## Context

Modelhub is a multi-provider aggregator. Each upstream (Black Forest Labs, Google AI, OpenAI, future fal/Replicate via Enterprise contracts) has a different API shape, auth model, request/response schema, error taxonomy, and async lifecycle. We need a single internal abstraction that the rest of the system (worker, wallet, catalog, API layer) talks to without knowing or caring which upstream is on the other side.

The interface we choose for this layer determines the velocity of every future model addition. Get it right, and adding model #11 takes a day. Get it wrong, and Sprint 3 (catalog expansion) is a series of refactors.

## Decision

`ProviderAdapter` is defined as a Go interface with **6 small, mandatory methods** plus a sentinel-error contract. The interface is **provider-agnostic** by construction — no fal-specific fields, no OpenAI-style tool-calling parameters, no Replicate-shaped predictions API leaking through.

```go
type ProviderAdapter interface {
    Key() ProviderKey
    Submit(ctx, model, params, idempotencyKey) (SubmitResult, error)
    Poll(ctx, ref) (PollResult, error)
    Cancel(ctx, ref) error
    EstimateCost(model, params) (CostUSD, error)
    Capabilities() ProviderCaps
}
```

Implementations live in subpackages: `internal/adapter/bfl`, `internal/adapter/googleai`, `internal/adapter/openai`. Each implementation registers itself with the registry at package `init()` (registry ships in S2.5).

## Why these methods specifically

| Method | Why |
|--------|-----|
| `Submit` | The canonical "kick off a generation" call. Returns an UpstreamRef for async or an inline NormalizedResult for sync. Both shapes share one return type to keep adapters honest about which kind they are. |
| `Poll` | Async lifecycle. Webhook-capable adapters STILL implement Poll because webhooks miss in production and the worker reconciler needs a fallback (per ADR-004). |
| `Cancel` | Optional via `ProviderCaps.SupportsCancel`. We didn't make it variadic / optional in the interface because Go interfaces don't have ergonomic optional methods, and `ErrUnsupported` is a clean idiom. |
| `EstimateCost` | The wallet's Hold sizing comes from here. Splitting estimate from actual cost is critical because most upstreams don't pre-quote — we must guess. Settle reconciles. |
| `Capabilities` | Static feature flags. Lets the worker decide webhook-vs-poll and the UI decide whether to show a Cancel button. Static (not dynamic-per-call) because dynamic feature negotiation is a complexity tar pit. |
| `Key` | Trivial; needed for registry lookup. |

## Why NOT these alternatives

### "Just use OpenAI Chat Completion as the universal interface"
Tempting because new-api inherits this assumption. **Rejected**: OpenAI Chat Completion has no concept of asynchronous tasks, doesn't model image/video/audio results, requires a forced-fit translation layer for non-LLM models, and bakes streaming-text assumptions into the wire format. Future Sora-text-to-video will need an envelope that doesn't exist in Chat Completion. We pay this cost forever for a familiarity-with-the-LLM-world ROI.

### "Plugins (Go shared objects / WASM / scripting)"
Tempting because adapter velocity matters. **Rejected**: Go's plugin system is unstable cross-platform, WASM in Go is still rough, and scripting (Lua/Starlark) loses static type safety which is exactly the property we want for money-handling code. Adapter PRs as compiled Go code are the safest substrate for now. Revisit if we cross 50 adapters and code review becomes the bottleneck.

### "One mega-method: `Run(ctx, request) (response, error)`"
Tempting for simplicity. **Rejected**: Conflates Submit and Poll, hides the async lifecycle, makes worker scheduling impossible to model without leaky time.Sleep loops inside adapters. Splitting Submit + Poll is what makes the worker layer reusable across all adapters.

### "Streaming-by-default interface"
Tempting because LLM is streaming-native. **Rejected for now**: only LLM is streaming for Sprint 1 (no streaming image/video models from BFL/Google/OpenAI yet). Forcing every adapter to expose a streaming surface adds complexity for zero current benefit. ProviderCaps.SupportsStreaming flags it as opt-in for the few adapters that need it.

## Consequences

### Positive
- Adding a new provider = one PR adding a subpackage with one ProviderAdapter implementation. Mock adapters in tests prove the interface holds before any real upstream is wired.
- Money-handling code (wallet) talks to the registry, not directly to provider clients — no risk of a wallet bug being provider-specific.
- The OpenAPI envelope (ADR-009) and this interface evolved in lockstep; they share the same NormalizedResult shape, eliminating translation drift.

### Negative
- Adapter implementations carry the burden of translation between our canonical Params and upstream-specific request bodies. This is the right place for that complexity (it's the layer that's allowed to know about upstream quirks) but it does mean each adapter PR is non-trivial.
- `ProviderCaps` static-only design means we can't represent "this provider sometimes supports streaming for some models." If we hit this case, we'd add per-model capabilities to ModelManifest rather than mutate this interface.

### Neutral
- The interface is small enough that mock implementations (mock_sync.go, mock_async.go, ships in S2 follow-up) are practical. This is critical for unit-testing wallet and worker without provider keys.

## Anti-pattern guard for reviewers

Code review of any adapter PR MUST check:

1. **No upstream provider name in `internal/adapter/provider.go`.** This file stays vendor-neutral.
2. **No NormalizedResult fields specific to one provider.** If you find yourself adding `OutputType = "fal_image_with_lora_metadata"`, stop — that belongs in `Metadata map[string]any`, not as a top-level type.
3. **No upstream URL leaking past NormalizeResult.** S9.5 asset worker rewrites all URLs.
4. **No JSON-Schema validation in adapters.** Trust the API-layer validation.
5. **No internal time.Sleep loops in Poll.** Worker schedules calls; backoff is the worker's responsibility.

## References

- BLUEPRINT.md §S2 (this step's full task list)
- ADR-004 (async task lifecycle)
- ADR-009 (unified `/v1/generations` envelope)
- ADR-010 (asset hosting before Settle)
- ADR-018 (no-passthrough red line — informed our decision to never expose upstream-shaped routes)

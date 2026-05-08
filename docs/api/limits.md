# Limits

## Per-user concurrent task ceiling (AP-15)

Default: **5 in-flight tasks** per user across all models. Submit attempts beyond this fail with HTTP 429:

```json
{ "error": { "code": "rate_limit", "message": "Concurrent task ceiling (5) exceeded. Wait for a task to complete or contact support to raise the ceiling." } }
```

The count includes any task in `held / submitted / running` state — i.e., not yet terminal. As soon as a task transitions to `succeeded / failed / timed_out / cancelled`, the slot opens up.

To raise the ceiling for an account, an admin sets `users.concurrent_ceiling`.

## Per-request cost cap (AP-16)

Default: **$5.00 per single request**. If a model's `EstimateCost(params)` returns a value > $5, the request fails with HTTP 400:

```json
{ "error": { "code": "payment", "message": "Estimated cost ($7.50) exceeds per-request ceiling ($5.00). Reduce parameters or set the high-cost flag on your account." } }
```

This catches accidental runaways (e.g., a 30-second video at $0.50/sec = $15, blocked by default). To opt in to high-cost requests, an admin sets `users.high_cost_flag = true`.

The hard ceiling for the entire wallet is `MaxCostUSD = $1000` per single Hold (defined in `internal/adapter/provider.go`). Even with the high-cost flag, no single request can exceed that.

## Rate limits per provider

Each upstream provider has its own concurrency cap. Modelhub respects them:

| Provider | Default per-account | Notes |
|----------|---------------------|-------|
| Black Forest Labs | 2 concurrent (16 with $1k+ deposit) | Per BFL FAQ |
| Google Vertex AI | Project quota | Configurable in GCP console |
| OpenAI gpt-image-1 | Tier-based, ~50 default | Visit OpenAI rate limits page for your tier |

If a provider returns 429, modelhub backs off with exponential jitter (5s/10s/20s/40s/60s capped). Your task remains in flight; you don't get charged extra for the wait.

## Idempotency window

60-second buckets per `(account, model, canonical_params)`. Submitting the same payload within 60s returns the existing task ID. After 60s, the same payload creates a new task.

This prevents accidental double-submission from network retries while still allowing legitimate "I really do want a fresh result" workflows to succeed by waiting.

## Asset URL TTL

All output URLs returned by modelhub are **our CDN URLs**, NOT upstream URLs (per AP-19). The retention policy:

- Default: 90 days from generation
- Configurable per user / per plan
- Storage cap: 5 GB per user (soft), with admin alert at 80%

If a generation succeeds but our asset worker fails to download from the upstream (e.g., BFL's 10-minute upstream URL TTL expired before our 3 retries succeeded), the task transitions to `asset_lost` state and you receive a **partial refund** (asset cost portion only; compute portion is retained because we already paid the upstream).

## How to interpret 429 responses

```http
HTTP/1.1 429 Too Many Requests
Retry-After: 5
Content-Type: application/json

{ "error": { "code": "rate_limit", "message": "..." } }
```

Honor `Retry-After` (in seconds). Modelhub may emit it on:

- Concurrent ceiling hit (Retry-After ≈ a few seconds, time for an in-flight to complete)
- Provider 429 propagated (Retry-After matches the provider's hint)
- Per-IP rate limit (rare, only if you're hammering /v1/generations from a single source)

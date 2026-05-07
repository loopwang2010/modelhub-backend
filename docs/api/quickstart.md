# Modelhub API — Quickstart

This guide walks you from zero to your first generation in five minutes. Three MVP models are available at launch:

- **Flux Pro 1.1** — text-to-image (sync, ~5–30s)
- **Veo 3.0 Pro** — text-to-video (async, 1–5min for a 5-second clip)
- **GPT-image-1 edit** — image-in → image-out (sync, ~5–30s)

## 1. Create an account

```http
POST /v1/auth/register
Content-Type: application/json

{
  "email": "you@example.com",
  "password": "<at least 12 chars>"
}
```

The response sets a `modelhub_session` HttpOnly cookie. **You never see the JWT** — modelhub never returns it in a body and never accepts it from headers. All API calls below assume the cookie travels via a cookie-jar (curl) or `withCredentials: true` (browser).

## 2. Get credit

The MVP has **no payment gateway** — credits are granted by an admin. Ask your admin to run:

```http
POST /admin/wallet/topup
{
  "user_id": "<your-user-id>",
  "amount_usd": 10.00,
  "note": "MVP grant"
}
```

Verify your balance:

```http
GET /v1/wallet/balance
→ { "balance_micro_usd": 10000000 }   // i.e. $10.00 (1_000_000 micro = $1)
```

## 3. Inspect the catalog

```http
GET /v1/models
→ { "data": [
      { "key": "flux-pro-1.1", "modality": "image", "task_kind": "async", ... },
      { "key": "veo-3.0-pro",  "modality": "video", "task_kind": "async", ... },
      { "key": "gpt-image-1-edit", "modality": "edit", "task_kind": "sync", ... }
    ] }
```

Each model entry includes `input_schema` (JSON Schema for `params`) and `price_formula` (human-readable cost description).

## 4. Make your first generation

The endpoint is **always** `POST /v1/generations` regardless of modality (per ADR-009 polymorphic envelope). The request shape is:

```json
{
  "model": "<model.key from /v1/models>",
  "params": { ...validated against model.input_schema... }
}
```

Sync models (Flux, GPT-image) return the result inline:

```json
{ "id": "gen_xxx", "status": "succeeded", "output": { "type": "image_url", "url": "..." }, ... }
```

Async models (Veo3) return a queued task:

```json
{ "id": "gen_xxx", "status": "queued", ... }
```

For async, poll `GET /v1/generations/{id}` until `status` is terminal:

```
queued → running → succeeded | failed
```

See `samples/{model}/` for runnable curl/Python/Node examples for each model.

## 5. Understand the response envelope (ADR-009)

```jsonc
{
  "id": "gen_01HX...",
  "model": "flux-pro-1.1",
  "status": "queued | running | succeeded | failed",
  "modality": "image | video | audio | edit",
  "task_kind": "sync | async",
  "created_at": "...",
  "completed_at": "..." | null,

  // Always our CDN URL — never an upstream URL (per ADR-018 + AP-19).
  "output": { "type": "image_url", "url": "https://cdn.modelhub.../...", "metadata": { ... } } | null,

  // ErrorClass taxonomy below.
  "error":  { "code": "auth | payment | rate_limit | content_policy | upstream | timeout | unknown", "message": "..." } | null,

  // Held vs settled vs refunded credits in micro-USD.
  "credits": { "held": 250000, "settled": 230000, "refunded": 20000 }
}
```

Rules:
- `output` is set only when `status == "succeeded"`.
- `error` is set only when `status == "failed"`.
- `credits.held` is your reserved escrow at submit time; `settled` is what you're actually charged after success; `refunded = held - settled` (or `held` for a full failure refund).
- The output `type` discriminates the polymorphic shape — image/video/audio yield a `url`; text yields content in `metadata.text`; base64 yields content in `metadata.base64`.

## 6. Idempotency

Submitting twice with the same `(account, model, params)` within 60 seconds returns the same task ID — no double-charge. To force a new task, change a parameter or wait 60s.

## 7. Where to next

- `auth.md` — login/logout/refresh and 401 handling
- `limits.md` — concurrent task ceiling, per-request cost cap, rate limits
- `webhooks.md` — webhook subscriptions for async completions
- `samples/{model}/{lang}/` — runnable code samples
- Live API reference (Redoc): open `index.html` or visit `/docs` on the running backend

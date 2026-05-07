# Webhooks

For async models (Veo3, sometimes BFL Flux), polling `GET /v1/generations/{id}` works but is wasteful. Subscribe to a webhook to be notified on completion.

> **MVP status:** webhook subscription via the API is in **Sprint 2** scope. The receiver is wired in S5; the subscription-management endpoints land later. Skip this section if you're shipping against the MVP.

## Subscription model (Sprint 2)

```http
POST /v1/webhooks
{
  "url": "https://your-app.example.com/modelhub-callback",
  "secret": "<your HMAC secret, 32+ random bytes>",
  "events": ["task.succeeded", "task.failed", "task.asset_lost"]
}
```

Subscriptions are scoped to your account. Subscribe once; receive callbacks for any task you submit.

## Payload shape

```jsonc
{
  "id": "evt_xxx",
  "type": "task.succeeded",
  "task_id": "gen_xxx",
  "occurred_at": "...",
  "data": {
    // Mirrors the GET /v1/generations/{id} envelope shape.
    "id": "gen_xxx",
    "model": "veo-3.0-pro",
    "status": "succeeded",
    "output": { "type": "video_url", "url": "https://cdn.modelhub.../...", ... },
    "credits": { "held": 2500000, "settled": 2470000, "refunded": 30000 }
  }
}
```

## Signature verification (HMAC-SHA256)

Every callback carries an HMAC signature header:

```
X-Modelhub-Signature: t=1715000000,v1=<hex>
```

The `v1` value is `HMAC-SHA256(your_secret, t + "." + raw_request_body)`. Verify in code:

```python
import hmac, hashlib

def verify(secret: bytes, headers: dict, raw_body: bytes) -> bool:
    sig = headers.get("X-Modelhub-Signature", "")
    parts = dict(p.split("=", 1) for p in sig.split(","))
    t, v1 = parts.get("t", ""), parts.get("v1", "")
    expected = hmac.new(secret,
                        f"{t}.".encode() + raw_body,
                        hashlib.sha256).hexdigest()
    if not hmac.compare_digest(v1, expected):
        return False
    # Reject replays older than 5 minutes
    if abs(time.time() - int(t)) > 300:
        return False
    return True
```

Use `hmac.compare_digest` (Python) / `crypto.timingSafeEqual` (Node) — string `==` is timing-attack-vulnerable.

## URL token unguessability (AP-18)

Modelhub's INGRESS webhook URLs (the URLs upstream providers POST to when calling modelhub for async completion) include a 256-bit random per-task token:

```
POST https://api.modelhub.example.com/v1/webhooks/<provider_key>/<task_id>/<256-bit-token>
```

The token is generated when the task is created. Without it, the URL is a 404 — even if you know the provider and task ID. This blocks attackers from forging "succeeded" webhooks.

The same protection applies to YOUR receiver: when you register your subscription URL, modelhub never tells anyone else what you registered. Your secret is only stored hashed.

## Idempotency

Webhooks are at-least-once. Receiving the same event twice is normal. Use `evt_xxx` as your dedup key:

```python
seen = set()
def handle(evt):
    if evt["id"] in seen:
        return  # dedup
    seen.add(evt["id"])
    # process...
```

The FSM also guards against double-processing internally — receiving the same `task.succeeded` twice doesn't double-settle the wallet.

## Why prefer webhooks over polling

A 5-minute Veo3 generation polled every 30s = 10 GET requests = 10 × backend round-trips. With a webhook, it's 0 (until you receive the callback).

Polling is the safety net (ADR-004): even with webhooks subscribed, modelhub's own worker still polls upstream as a redundancy. You don't need to poll from your client side once you have a webhook.

## Failure handling

If your endpoint returns non-2xx, modelhub retries with exponential backoff: 1m, 5m, 30m, 2h, 12h. After 5 failed deliveries, the subscription is auto-paused and an admin alert fires. To resume, hit `POST /v1/webhooks/{id}/resume`.

A 410 response (Gone) tells modelhub to immediately deactivate the subscription — use this when you're decommissioning the URL.

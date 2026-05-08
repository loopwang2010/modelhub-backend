# Auth

Modelhub uses **HttpOnly cookie session** authentication. The JWT lives in the cookie, never in a JSON body, never in localStorage.

## Endpoints

| Method | Path | Purpose |
|--------|------|---------|
| `POST` | `/v1/auth/register` | Create account + auto-create wallet |
| `POST` | `/v1/auth/login` | Issue session cookie |
| `POST` | `/v1/auth/logout` | Clear session cookie |
| `GET`  | `/v1/auth/me` | Identity probe (use to test if cookie is valid) |

## Cookie shape

```
Set-Cookie: modelhub_session=<opaque>; HttpOnly; Secure; SameSite=Lax; Path=/; Max-Age=86400
```

- `HttpOnly` — JavaScript can't read it. Mitigates XSS-credential-theft.
- `Secure` — only sent over HTTPS in production.
- `SameSite=Lax` — sent on top-level navigations, blocked on most cross-site POSTs.
- `Max-Age=86400` — 24-hour sliding window. No refresh tokens in MVP — re-login when expired.

## Curl: login + reuse cookie

```bash
# 1. Login (saves cookie to jar)
curl -c /tmp/mh.cookies -X POST https://modelhub.example.com/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"you@example.com","password":"..."}'

# 2. All subsequent requests reuse the cookie
curl -b /tmp/mh.cookies https://modelhub.example.com/v1/auth/me
curl -b /tmp/mh.cookies https://modelhub.example.com/v1/wallet/balance
```

## Python (requests): persistent session

```python
import requests
session = requests.Session()
session.post("https://modelhub.example.com/v1/auth/login",
             json={"email": "you@example.com", "password": "..."})
me = session.get("https://modelhub.example.com/v1/auth/me").json()
print(me)
# Subsequent session.* calls automatically attach the cookie
```

## Node (fetch): credentials: 'include'

```js
const base = "https://modelhub.example.com";

await fetch(`${base}/v1/auth/login`, {
  method: "POST",
  headers: { "Content-Type": "application/json" },
  credentials: "include",   // critical — without this the cookie is dropped
  body: JSON.stringify({ email: "you@example.com", password: "..." })
});

const me = await fetch(`${base}/v1/auth/me`, { credentials: "include" });
```

In the browser, `axios.create({ withCredentials: true })` is the equivalent.

## 401 handling

A 401 means the cookie is missing, expired, or invalidated. The recommended pattern:

```js
// Pseudocode for axios interceptor
axios.interceptors.response.use(r => r, err => {
  if (err.response?.status === 401) {
    location.href = '/login';
  }
  return Promise.reject(err);
});
```

Server-to-server clients should call `/v1/auth/login` again and retry.

## Registration auto-creates a wallet

When you register, the server atomically creates both your user row AND a `wallet_account` row of kind `user_wallet` (per ADR-013 typed `AccountID`). The wallet starts at $0.00. Ask an admin to top you up via `POST /admin/wallet/topup`.

## What modelhub will NEVER ask for

- Your provider keys (BFL, OpenAI, Google) — modelhub holds its own master keys per ADR-001 + ADR-016
- Your JWT (it lives in the cookie; modelhub doesn't accept it in a header)
- Anything via localStorage — AP-4 forbids it. If a future client suggests pasting an API key into a modal, that's a regression bug.

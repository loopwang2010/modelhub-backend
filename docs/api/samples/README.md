# Code samples

Copy-paste-runnable samples for each Sprint-1 MVP model in three
languages. Each directory is self-contained; pick the language you want
and run the example after exporting `MODELHUB_EMAIL` /
`MODELHUB_PASSWORD` (or pre-creating a `modelhub-cookies.txt` cookie
jar for the curl flavor).

| Model | Modality | Task kind | Path |
|---|---|---|---|
| `flux-pro-1.1` | image | sync | `flux/{curl,python,node}` |
| `veo-3.0-pro` | video | async (poll) | `veo3/{curl,python,node}` |
| `gpt-image-1-edit` | edit | async + upload | `gpt-image/{curl,python,node}` |

## What every sample assumes

1. The backend is running. Defaults to `http://localhost:6666`; set
   `MODELHUB_BASE` to override.
2. You have a session. The samples include a one-shot login using
   `MODELHUB_EMAIL` / `MODELHUB_PASSWORD`; the curl flavor reads from a
   cookie jar (`./modelhub-cookies.txt`) instead.
3. Your wallet has credits. See `../quickstart.md` for the admin
   top-up path during MVP.

## Running

```bash
# Python — needs `pip install requests`
python flux/python/example.py "a forest at dawn"

# Node — needs Node 18+ (built-in fetch)
node flux/node/example.mjs "a forest at dawn"

# curl — needs curl + python (used as a tiny JSON parser)
bash flux/curl/example.sh "a forest at dawn"
```

The Python and Node samples log in with email + password each run; the
curl sample expects a cookie jar already on disk:

```bash
JAR=./modelhub-cookies.txt
curl -c "$JAR" -X POST http://localhost:6666/v1/auth/login \
    -H 'Content-Type: application/json' \
    -d '{"email":"alice@example.com","password":"correct horse battery staple"}'

bash flux/curl/example.sh "a forest at dawn"
```

## What each sample demonstrates

### Flux (`flux/`)

- The simplest possible call. Sync model, no polling.
- Uses default size (1024 × 1024).
- Surfaces 401 / 402 with helpful error messages mapped to the
  `error.code` taxonomy in `../quickstart.md`.

### Veo3 (`veo3/`)

- Async submit returning HTTP 202 with `status: queued`.
- Polling loop with gentle back-off (cap 30s) and a budget timeout.
- Demonstrates how `credits.held` becomes `credits.settled` once the
  task succeeds.

### GPT-image (`gpt-image/`)

- Pre-signed upload via `POST /v1/uploads`.
- Direct `PUT` of the source image to the signed URL.
- Async submit referencing `upload_id`, then polling.
- Shows how MIME validation flows through the upload mint.

## Cookies and Node fetch

Plain Node fetch does not maintain a cookie jar. The samples extract
the `Set-Cookie` from the login response and replay it manually as a
`Cookie:` header on later calls. For larger applications, use one of:

- `node-fetch-cookies` (drop-in for `fetch` with persistent jar)
- `axios` with `axios-cookiejar-support` + `tough-cookie`
- A `CookieAgent` from `undici` (bundled with Node)

The pattern in these samples works fine for short-lived scripts but
will silently break if the backend ever sets two cookies in one
response. Use a real cookie library for production code.

## Authentication failures

Every sample treats `401` as "your cookie is gone" and exits with a
clear message. The expected response is to re-login (CLI: re-run with
correct credentials; SPA: redirect to the login route).

`auth.md` covers this in detail.

## Cost-cap and rate-limit responses

The samples surface `402` and `429` as distinct error paths so you can
see them at the wire. See `../limits.md` for what they mean and how
much headroom your account currently has.

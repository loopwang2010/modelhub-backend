# Modelhub Deployment Guide (S13)

This guide covers deploying modelhub-backend + modelhub-web behind nginx with Postgres, Redis, and MinIO via Docker Compose. Suitable for a single-host MVP launch.

## Topology

```
                    ┌────── nginx (TLS, /docs, static frontend, /v1 proxy)
        :443 ───────│
                    └────── modelhub-backend (Go, :6666)
                                ├── postgres (wallet, task, …)
                                ├── redis (worker queue)
                                └── minio (S3-compatible)
```

## Prereqs

- Docker 24+ + Docker Compose v2
- A domain pointing at the host (cloudflare, etc.)
- TLS cert + key (Let's Encrypt or self-signed for dev)
- Provider credentials: `BFL_API_KEY`, `OPENAI_API_KEY`, GCP service account JSON

## First-time setup

```bash
# 1. Clone + branch
git clone https://github.com/loopwang2010/modelhub-backend.git
cd modelhub-backend
git checkout design/core-interfaces

# 2. Copy env template
cp .env.example .env
$EDITOR .env

# 3. Drop GCP service account JSON in place
mkdir -p secrets
cp /path/to/your/gcp-sa.json secrets/gcp-sa.json
chmod 600 secrets/gcp-sa.json

# 4. Drop TLS certs
mkdir -p deploy/nginx/certs
cp /path/to/fullchain.pem deploy/nginx/certs/modelhub.crt
cp /path/to/privkey.pem    deploy/nginx/certs/modelhub.key

# 5. Build modelhub-web frontend (separate repo)
cd ../modelhub-web
npm install
npm run build
cp -r .next/* ../modelhub-backend/deploy/static/   # adjust per Next.js output mode

cd ../modelhub-backend

# 6. Bring up the stack
docker compose up -d

# 7. Run migrations
docker compose exec modelhub-backend ./modelhub-migrate up

# 8. Seed the model catalog (3 MVP entries)
docker compose exec modelhub-backend ./modelhub-seed catalog --models flux-pro-1.1,veo-3.0-pro,gpt-image-1-edit

# 9. Create admin user manually (no payment gateway in MVP)
docker compose exec postgres psql -U modelhub -c \
  "UPDATE users SET is_admin = true WHERE email = 'you@example.com';"

# 10. Smoke test
MODELHUB_URL=https://modelhub.example.com \
  MH_EMAIL=you@example.com MH_PASSWORD=... \
  bash scripts/smoke.sh
```

## Production hardening checklist

- [ ] **TLS cert auto-renewal** — Let's Encrypt via certbot in a sidecar OR external load balancer with managed cert
- [ ] **HSTS preload** — already in nginx.conf, but only enable `preload` after 90 days of stable cert
- [ ] **Secrets rotation** — JWT_SECRET, WEBHOOK_HMAC_SECRET, provider keys. Document a 90-day rotation cadence.
- [ ] **Backups** — daily `pg_dump` to MinIO; weekly bucket replication off-host
- [ ] **Log shipping** — nginx + backend stdout JSON; ship via filebeat/fluentd to a central store
- [ ] **Metrics** — backend `/metrics` Prometheus endpoint (scope: Sprint 2)
- [ ] **Health checks** — `/healthz` + `/readyz` (the latter checks DB + Redis)
- [ ] **Admin role designation** — first user manually flagged via SQL; document the runbook
- [ ] **Disk quota monitoring** — minio bucket size cap, postgres data growth
- [ ] **Container update cadence** — base images updated monthly minimum

## Dev-vs-prod env parity

| Concern | Dev | Prod |
|---------|-----|------|
| TLS | self-signed in `deploy/nginx/certs/` | Let's Encrypt + HSTS |
| Object storage | MinIO local | MinIO self-hosted OR Cloudflare R2 OR AWS S3 |
| Provider keys | dev keys with low daily quota | rotated prod keys |
| Backups | none | daily pg_dump |
| Logs | stdout | shipped to log store |
| `DEV_MODE` | `=mock` to boot without keys | unset |
| `GOPROXY` | `goproxy.cn` for China devs | unset (default proxy.golang.org) |

## Open questions before production launch

These map to BLUEPRINT.md §7 questions:

| Q | Default if unanswered |
|---|----------------------|
| Welcome credit on register? | $0 — admin grants via top-up |
| Email verification on register? | Off — users active immediately |
| Final domain | `modelhub.example.com` placeholder |
| Object storage choice | MinIO local (cheap to start; migrate to R2/S3 later) |

## Troubleshooting

### Backend container fails to start

Check logs: `docker compose logs modelhub-backend`. Common causes:
- `DATABASE_URL` malformed → check `.env`
- Postgres not yet ready → wait 30s, restart
- Migrations not applied → run `./modelhub-migrate up` once

### `/v1/generations` returns 503

Backend can't reach upstream provider. Check:
- Provider API key in `.env`
- Network egress from container (China deployments need GOPROXY for build, plus outbound HTTPS to api.bfl.ai / OpenAI / Google)

### Asset URLs are upstream URLs (AP-19 violation)

The S9.5 asset worker isn't running OR its bucket isn't reachable. Check:
- `S3_ENDPOINT` resolves from inside the container (`docker compose exec modelhub-backend nslookup minio`)
- MinIO bucket `modelhub-outputs` exists (create manually first time)
- Logs for asset-worker errors

### Webhook ingress 401s on every callback

HMAC secret mismatch. Either:
- The provider's signing secret in your config doesn't match what they're signing with
- The webhook URL token is enumerable / leaked (rotate task tokens by recreating)

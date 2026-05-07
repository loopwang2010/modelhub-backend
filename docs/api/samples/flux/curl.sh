#!/usr/bin/env bash
# Flux Pro 1.1 — text-to-image — curl example
#
# Async model: submit returns "queued"; poll until succeeded.
# Prereq: you've already logged in and saved the cookie to /tmp/mh.cookies.

set -euo pipefail
BASE="${MODELHUB_URL:-https://modelhub.example.com}"

# 1. Submit
RESP=$(curl -sS -X POST "$BASE/v1/generations" \
  -b /tmp/mh.cookies \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "flux-pro-1.1",
    "params": {
      "prompt": "An origami fox in a foggy forest at dawn, cinematic",
      "aspect_ratio": "16:9",
      "num_images": 1
    }
  }')
TASK_ID=$(echo "$RESP" | jq -r '.id')
echo "Submitted: $TASK_ID"

# 2. Poll (60 attempts × 5s = 5 min ceiling — Flux usually returns in 5-30s)
for _ in $(seq 1 60); do
  STATUS_RESP=$(curl -sS "$BASE/v1/generations/$TASK_ID" -b /tmp/mh.cookies)
  STATUS=$(echo "$STATUS_RESP" | jq -r '.status')
  case "$STATUS" in
    succeeded)
      echo "Done."
      echo "$STATUS_RESP" | jq '{ url: .output.url, credits: .credits }'
      exit 0
      ;;
    failed)
      echo "Failed:"; echo "$STATUS_RESP" | jq '.error'
      exit 1
      ;;
    *)
      sleep 5
      ;;
  esac
done

echo "Timeout waiting for $TASK_ID" >&2
exit 1

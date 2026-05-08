#!/usr/bin/env bash
# Veo 3.0 Pro — text-to-video — curl example
#
# Async model. Veo3 produces 5-30 second clips, typically 1-5 minutes.
# Prereq: cookie jar at /tmp/mh.cookies, and your wallet has $5+ (Veo3 is pricey).

set -euo pipefail
BASE="${MODELHUB_URL:-https://modelhub.example.com}"

# Submit. duration_seconds=5 keeps the cost predictable (~$2.50 at $0.50/sec).
RESP=$(curl -sS -X POST "$BASE/v1/generations" \
  -b /tmp/mh.cookies \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "veo-3.0-pro",
    "params": {
      "prompt": "A drone shot orbiting a snow-covered cabin, golden hour light, cinematic",
      "duration_seconds": 5,
      "aspect_ratio": "16:9"
    }
  }')
TASK_ID=$(echo "$RESP" | jq -r '.id')
echo "Submitted: $TASK_ID (held $(echo "$RESP" | jq -r '.credits.held') micro-USD)"

# Poll up to 15 minutes (Veo3 SLA per S5-WORKER-DESIGN.md).
for i in $(seq 1 90); do
  STATUS_RESP=$(curl -sS "$BASE/v1/generations/$TASK_ID" -b /tmp/mh.cookies)
  STATUS=$(echo "$STATUS_RESP" | jq -r '.status')
  case "$STATUS" in
    succeeded)
      echo "Done after ${i} polls."
      echo "$STATUS_RESP" | jq '{ url: .output.url, credits: .credits }'
      exit 0
      ;;
    failed)
      echo "Failed:"; echo "$STATUS_RESP" | jq '.error'
      exit 1
      ;;
    *)
      [[ $((i % 3)) -eq 0 ]] && echo "  ...still ${STATUS} (poll $i/90)"
      sleep 10
      ;;
  esac
done

echo "Timeout — task still in flight after 15 min" >&2
echo "Check $BASE/v1/generations/$TASK_ID directly" >&2
exit 1

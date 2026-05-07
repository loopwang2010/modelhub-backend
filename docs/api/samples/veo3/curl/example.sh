#!/usr/bin/env bash
# veo-3.0-pro — async video generation via curl.
#
# Async model: submit returns 202 with status=queued and credits.held
# reserved against your wallet. Result becomes available via:
#   - polling GET /v1/generations/{id}, or
#   - webhook delivery (see docs/api/webhooks.md)
#
# This example polls. Default budget is 10 minutes with 5s intervals.
#
# Usage:
#   ./example.sh "a slow zoom-out from a city skyline"
#
# Defaults:
#   - BASE  → http://localhost:6666
#   - JAR   → ./modelhub-cookies.txt   (login first; see docs/api/auth.md)

set -euo pipefail

BASE="${MODELHUB_BASE:-http://localhost:6666}"
JAR="${MODELHUB_COOKIE_JAR:-./modelhub-cookies.txt}"
PROMPT="${1:-a slow zoom-out from a city skyline at golden hour}"
DURATION="${MODELHUB_VEO_DURATION:-5}"
POLL_INTERVAL="${MODELHUB_POLL_INTERVAL:-5}"
POLL_MAX_S="${MODELHUB_POLL_MAX_S:-600}"

if [[ ! -f "$JAR" ]]; then
    echo "no cookie jar at $JAR — login first via docs/api/auth.md" >&2
    exit 1
fi

# 1. Submit. Async returns 202 with status=queued.
SUBMIT=$(curl -sS -b "$JAR" -X POST "$BASE/v1/generations" \
    -H "Content-Type: application/json" \
    --data "$(cat <<EOF
{
    "model": "veo-3.0-pro",
    "params": {
        "prompt": "$PROMPT",
        "duration_seconds": $DURATION
    }
}
EOF
)")

GEN_ID=$(printf '%s' "$SUBMIT" | python -c "import json, sys; print(json.load(sys.stdin)['id'])")
echo "submitted: $GEN_ID"
echo "          (held=$(printf '%s' "$SUBMIT" | python -c 'import json,sys;print(json.load(sys.stdin)["credits"]["held"])' ) micro-USD)"

# 2. Poll until terminal.
START=$(date +%s)
while :; do
    NOW=$(date +%s)
    if (( NOW - START > POLL_MAX_S )); then
        echo "timeout after ${POLL_MAX_S}s — task still in flight; check later with:" >&2
        echo "    curl -b \"$JAR\" $BASE/v1/generations/$GEN_ID" >&2
        exit 3
    fi

    sleep "$POLL_INTERVAL"

    POLL=$(curl -sS -b "$JAR" "$BASE/v1/generations/$GEN_ID")
    STATUS=$(printf '%s' "$POLL" | python -c "import json, sys; print(json.load(sys.stdin)['status'])")
    echo "  ${STATUS} (elapsed $((NOW - START))s)"

    case "$STATUS" in
        succeeded)
            URL=$(printf '%s' "$POLL" | python -c "
import json, sys
out = json.load(sys.stdin)['output'] or {}
print(out.get('url',''))
")
            SETTLED=$(printf '%s' "$POLL" | python -c "
import json, sys
print(json.load(sys.stdin)['credits']['settled'])
")
            echo "video:   $URL"
            echo "settled: $SETTLED micro-USD"
            exit 0
            ;;
        failed|cancelled)
            ERR=$(printf '%s' "$POLL" | python -c "
import json, sys
e = json.load(sys.stdin).get('error') or {}
print(f\"{e.get('code','unknown')}: {e.get('message','')}\")
")
            echo "task ${STATUS}: $ERR" >&2
            exit 2
            ;;
        queued|running)
            continue
            ;;
        *)
            echo "unexpected status: $STATUS" >&2
            exit 4
            ;;
    esac
done

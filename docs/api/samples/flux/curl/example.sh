#!/usr/bin/env bash
# flux-pro-1.1 — sync image generation via curl.
#
# Sync model: the request blocks for ~5-10s and returns the finished
# image inline. No polling needed.
#
# Usage:
#   ./example.sh "a forest at dawn"
#
# Defaults:
#   - BASE  → http://localhost:6666
#   - JAR   → ./modelhub-cookies.txt   (login first; see docs/api/auth.md)

set -euo pipefail

BASE="${MODELHUB_BASE:-http://localhost:6666}"
JAR="${MODELHUB_COOKIE_JAR:-./modelhub-cookies.txt}"
PROMPT="${1:-a forest at dawn, painterly, soft mist}"
WIDTH="${MODELHUB_FLUX_WIDTH:-1024}"
HEIGHT="${MODELHUB_FLUX_HEIGHT:-1024}"

if [[ ! -f "$JAR" ]]; then
    echo "no cookie jar at $JAR — login first via docs/api/auth.md, e.g.:" >&2
    echo "  curl -c \"$JAR\" -X POST $BASE/v1/auth/login \\" >&2
    echo "       -H 'Content-Type: application/json' \\" >&2
    echo "       -d '{\"email\":\"…\",\"password\":\"…\"}'" >&2
    exit 1
fi

# Submit. Sync model returns 200 with status=succeeded and output populated.
RESP=$(curl -sS -b "$JAR" -X POST "$BASE/v1/generations" \
    -H "Content-Type: application/json" \
    --data "$(cat <<EOF
{
    "model": "flux-pro-1.1",
    "params": {
        "prompt": "$PROMPT",
        "width": $WIDTH,
        "height": $HEIGHT
    }
}
EOF
)")

# Pretty-print the envelope. jq is optional; fall back to raw if absent.
if command -v jq >/dev/null 2>&1; then
    echo "$RESP" | jq .
else
    echo "$RESP"
fi

# Extract the output URL with python (universally available; no jq needed).
URL=$(printf '%s' "$RESP" | python -c "
import json, sys
env = json.load(sys.stdin)
if env.get('status') != 'succeeded':
    print('not succeeded:', env.get('status'), file=sys.stderr)
    sys.exit(2)
out = env.get('output') or {}
if out.get('type') != 'image_url':
    print('unexpected output type:', out.get('type'), file=sys.stderr)
    sys.exit(2)
print(out['url'])
")

echo "image: $URL"

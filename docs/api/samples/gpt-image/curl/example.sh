#!/usr/bin/env bash
# gpt-image-1-edit — image edit via curl, with pre-signed upload.
#
# Three steps:
#   1. POST /v1/uploads → get a pre-signed PUT URL.
#   2. PUT the source image bytes directly to the upload URL.
#   3. POST /v1/generations referencing the resulting upload_id.
#
# Step 3 is async; we poll for completion the same way as the Veo
# example.
#
# Usage:
#   ./example.sh ./cat.png "add a small wizard hat"
#
# Notes on AP-17 (source-upload validation):
#   - Allowed content types: image/{jpeg,png,webp,gif}.
#   - Size cap: 50 MiB per upload (tighter caps may apply per-model).
#   - The backend MIME-sniffs after upload; a mismatched extension is rejected.

set -euo pipefail

BASE="${MODELHUB_BASE:-http://localhost:6666}"
JAR="${MODELHUB_COOKIE_JAR:-./modelhub-cookies.txt}"
SRC="${1:?usage: $0 <source-image> <prompt>}"
PROMPT="${2:?usage: $0 <source-image> <prompt>}"
POLL_INTERVAL="${MODELHUB_POLL_INTERVAL:-5}"
POLL_MAX_S="${MODELHUB_POLL_MAX_S:-300}"

if [[ ! -f "$JAR" ]]; then
    echo "no cookie jar at $JAR — login first via docs/api/auth.md" >&2
    exit 1
fi
if [[ ! -f "$SRC" ]]; then
    echo "source image not found: $SRC" >&2
    exit 1
fi

# Detect content type from extension. Real clients should sniff for safety.
case "$SRC" in
    *.png)  CT=image/png ;;
    *.jpg|*.jpeg) CT=image/jpeg ;;
    *.webp) CT=image/webp ;;
    *.gif)  CT=image/gif ;;
    *) echo "unsupported source extension: $SRC" >&2; exit 1 ;;
esac

SIZE=$(wc -c < "$SRC" | tr -d ' ')

# 1. Mint a pre-signed PUT URL.
UPLOAD_RES=$(curl -sS -b "$JAR" -X POST "$BASE/v1/uploads" \
    -H "Content-Type: application/json" \
    --data "$(cat <<EOF
{"content_type":"$CT","size_bytes":$SIZE,"filename":"$(basename "$SRC")"}
EOF
)")
UPLOAD_URL=$(printf '%s' "$UPLOAD_RES" | python -c "import json,sys; print(json.load(sys.stdin)['upload_url'])")
UPLOAD_ID=$(printf  '%s' "$UPLOAD_RES" | python -c "import json,sys; print(json.load(sys.stdin)['upload_id'])")
echo "upload_id: $UPLOAD_ID"

# 2. PUT the bytes. The pre-signed URL embeds whatever extra headers the
#    backend requires; replay them via the `headers` map for stricter
#    upstreams. For the dev MinIO target, Content-Type is enough.
curl -sS -X PUT --data-binary "@$SRC" \
    -H "Content-Type: $CT" \
    "$UPLOAD_URL" >/dev/null
echo "uploaded:  $SIZE bytes"

# 3. Submit the edit. Async: returns 202 with status=queued.
SUBMIT=$(curl -sS -b "$JAR" -X POST "$BASE/v1/generations" \
    -H "Content-Type: application/json" \
    --data "$(cat <<EOF
{
    "model": "gpt-image-1-edit",
    "params": {
        "prompt": "$PROMPT",
        "image": { "upload_id": "$UPLOAD_ID" }
    }
}
EOF
)")
GEN_ID=$(printf '%s' "$SUBMIT" | python -c "import json,sys; print(json.load(sys.stdin)['id'])")
echo "submitted: $GEN_ID"

# 4. Poll until terminal.
START=$(date +%s)
while :; do
    NOW=$(date +%s)
    if (( NOW - START > POLL_MAX_S )); then
        echo "timeout after ${POLL_MAX_S}s — task still in flight" >&2
        exit 3
    fi
    sleep "$POLL_INTERVAL"
    POLL=$(curl -sS -b "$JAR" "$BASE/v1/generations/$GEN_ID")
    STATUS=$(printf '%s' "$POLL" | python -c "import json,sys; print(json.load(sys.stdin)['status'])")
    echo "  ${STATUS} (elapsed $((NOW - START))s)"

    case "$STATUS" in
        succeeded)
            URL=$(printf '%s' "$POLL" | python -c "
import json, sys
out = json.load(sys.stdin)['output'] or {}
print(out.get('url',''))
")
            echo "edited:  $URL"
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
        queued|running) ;;
        *) echo "unexpected status: $STATUS" >&2; exit 4 ;;
    esac
done

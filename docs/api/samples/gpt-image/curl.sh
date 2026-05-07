#!/usr/bin/env bash
# GPT-image-1 edit — image-in → image-out — curl example
#
# Sync model with two-step upload pattern (per ADR-009 — JSON-only API surface):
#   1. POST /v1/uploads → get pre-signed PUT URL
#   2. PUT the image binary to that URL
#   3. POST /v1/generations with params.image_id referencing the upload

set -euo pipefail
BASE="${MODELHUB_URL:-https://modelhub.example.com}"
SOURCE_IMAGE="${1:-/path/to/source.png}"

if [[ ! -f "$SOURCE_IMAGE" ]]; then
  echo "Usage: $0 /path/to/source.png" >&2
  exit 1
fi

SIZE=$(stat -c%s "$SOURCE_IMAGE" 2>/dev/null || stat -f%z "$SOURCE_IMAGE")

# 1. Reserve an upload slot
UPLOAD=$(curl -sS -X POST "$BASE/v1/uploads" \
  -b /tmp/mh.cookies \
  -H 'Content-Type: application/json' \
  -d "{ \"content_type\": \"image/png\", \"size_bytes\": $SIZE, \"filename\": \"source.png\" }")

UPLOAD_URL=$(echo "$UPLOAD" | jq -r '.upload_url')
UPLOAD_ID=$(echo "$UPLOAD" | jq -r '.upload_id')

# 2. Upload the image
curl -sS -X PUT "$UPLOAD_URL" \
  -H 'Content-Type: image/png' \
  --data-binary "@$SOURCE_IMAGE"

echo "Uploaded as $UPLOAD_ID"

# 3. Submit (sync — returns inline)
RESP=$(curl -sS -X POST "$BASE/v1/generations" \
  -b /tmp/mh.cookies \
  -H 'Content-Type: application/json' \
  -d "{
    \"model\": \"gpt-image-1-edit\",
    \"params\": {
      \"prompt\": \"Make this look like a watercolor painting\",
      \"image_id\": \"$UPLOAD_ID\",
      \"size\": \"1024x1024\"
    }
  }")

STATUS=$(echo "$RESP" | jq -r '.status')
if [[ "$STATUS" == "succeeded" ]]; then
  echo "Done."
  echo "$RESP" | jq '{ url: .output.url, credits: .credits }'
else
  echo "Failed:"; echo "$RESP" | jq '.error'
  exit 1
fi

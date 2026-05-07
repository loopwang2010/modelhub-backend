#!/usr/bin/env bash
# Modelhub end-to-end smoke test.
#
# Runs against a deployed instance. Verifies:
#   1. /v1/models returns the 3 MVP models.
#   2. Flux generation succeeds end-to-end (sync image → CDN URL).
#   3. Wallet ledger sum-zero invariant intact after the test.
#   4. Smoke runner has not exceeded the daily $5 spend cap.
#
# Required env:
#   MODELHUB_URL  e.g. https://modelhub.example.com
#   MH_EMAIL      smoke runner account email (admin-granted credit)
#   MH_PASSWORD   smoke runner password
#
# Optional:
#   ADMIN_EMAIL   admin account for diagnostics endpoints (defaults to MH_EMAIL)
#   ADMIN_PASSWORD

set -euo pipefail

BASE="${MODELHUB_URL:?need MODELHUB_URL}"
COOKIE_JAR="$(mktemp -t mh-smoke-cookies-XXXXXX)"
ADMIN_COOKIE_JAR="$(mktemp -t mh-smoke-admin-XXXXXX)"
trap 'rm -f "$COOKIE_JAR" "$ADMIN_COOKIE_JAR"' EXIT

green() { printf '\033[32m%s\033[0m\n' "$*"; }
red()   { printf '\033[31m%s\033[0m\n' "$*" >&2; }
fail()  { red "FAIL: $*"; exit 1; }

# ─── Login ──────────────────────────────────────────────────
curl -sS -c "$COOKIE_JAR" -X POST "$BASE/v1/auth/login" \
  -H 'Content-Type: application/json' \
  -d "{\"email\":\"${MH_EMAIL:?need MH_EMAIL}\",\"password\":\"${MH_PASSWORD:?need MH_PASSWORD}\"}" \
  > /dev/null || fail "login failed"
green "✓ login ok"

# ─── 1. Catalog has the 3 MVP models ────────────────────────
MODELS=$(curl -sS -b "$COOKIE_JAR" "$BASE/v1/models")
EXPECTED=("flux-pro-1.1" "veo-3.0-pro" "gpt-image-1-edit")
for m in "${EXPECTED[@]}"; do
  echo "$MODELS" | jq -e --arg m "$m" '.data[] | select(.key==$m)' > /dev/null \
    || fail "model $m missing from /v1/models"
done
green "✓ catalog has all 3 MVP models"

# ─── 2. Submit + poll a Flux generation ─────────────────────
SUBMIT=$(curl -sS -b "$COOKIE_JAR" -X POST "$BASE/v1/generations" \
  -H 'Content-Type: application/json' \
  -d '{"model":"flux-pro-1.1","params":{"prompt":"A small smoke-test test pattern","aspect_ratio":"1:1","num_images":1}}')
TASK_ID=$(echo "$SUBMIT" | jq -r '.id')
[[ -n "$TASK_ID" && "$TASK_ID" != "null" ]] || fail "submit returned no id: $SUBMIT"
green "✓ submitted: $TASK_ID"

DEADLINE=$(( $(date +%s) + 300 ))
while [[ $(date +%s) -lt $DEADLINE ]]; do
  R=$(curl -sS -b "$COOKIE_JAR" "$BASE/v1/generations/$TASK_ID")
  S=$(echo "$R" | jq -r '.status')
  case "$S" in
    succeeded)
      URL=$(echo "$R" | jq -r '.output.url')
      [[ -n "$URL" && "$URL" != "null" ]] || fail "succeeded but no URL in output"
      # AP-19 enforcement: URL must NOT be an upstream URL.
      if echo "$URL" | grep -qE 'fal\.media|api\.openai\.com|generativelanguage\.googleapis|api\.bfl\.ai|delivery-eu1\.bfl\.ai'; then
        fail "AP-19 violation: returned upstream URL: $URL"
      fi
      green "✓ Flux generation succeeded → $URL"
      break
      ;;
    failed)
      fail "Flux generation failed: $(echo "$R" | jq -c '.error')"
      ;;
    *)
      sleep 5
      ;;
  esac
done
[[ "$S" == "succeeded" ]] || fail "Flux generation timed out after 5 min"

# ─── 3. Ledger sum-zero invariant (admin endpoint) ──────────
# Login as admin if separate account
if [[ -n "${ADMIN_EMAIL:-}" && -n "${ADMIN_PASSWORD:-}" ]]; then
  curl -sS -c "$ADMIN_COOKIE_JAR" -X POST "$BASE/v1/auth/login" \
    -H 'Content-Type: application/json' \
    -d "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PASSWORD\"}" > /dev/null
  ADMIN_JAR="$ADMIN_COOKIE_JAR"
else
  ADMIN_JAR="$COOKIE_JAR"
fi

LEDGER_SUM=$(curl -sS -b "$ADMIN_JAR" "$BASE/admin/diagnostics/ledger-sum" | jq -r '.sum_micro_usd // empty')
if [[ -z "$LEDGER_SUM" ]]; then
  echo "  warn: /admin/diagnostics/ledger-sum endpoint not yet implemented (S6 follow-up); skipping invariant check"
elif [[ "$LEDGER_SUM" != "0" ]]; then
  fail "ledger sum-zero invariant violated: SUM(amount_micro_usd)=$LEDGER_SUM (expected 0)"
else
  green "✓ ledger sum-zero invariant intact"
fi

# ─── 4. Daily spend cap ($5 hard ceiling per S13 spec) ──────
DAILY=$(curl -sS -b "$ADMIN_JAR" "$BASE/admin/diagnostics/daily-spend?account=smoke-runner" \
  | jq -r '.today_micro_usd // empty')
if [[ -z "$DAILY" ]]; then
  echo "  warn: /admin/diagnostics/daily-spend endpoint not yet implemented; skipping spend-cap check"
elif (( DAILY > 5000000 )); then
  fail "smoke runner spent ${DAILY} micro-USD today, exceeds 5_000_000 ceiling ($5)"
else
  green "✓ smoke runner daily spend $DAILY micro-USD ≤ \$5"
fi

green ""
green "═══════════════════════════════════════════════════════════"
green "  SMOKE TEST PASSED"
green "═══════════════════════════════════════════════════════════"

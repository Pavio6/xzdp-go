#!/usr/bin/env bash
set -euo pipefail

BASE_URL="${BASE_URL:-http://127.0.0.1:8081}"
VOUCHER_ID="${VOUCHER_ID:-12}"
TOKEN="${TOKEN:-e5531507-af63-4f15-8ea6-72dcf383fa2c}"
LOG_FILE="${LOG_FILE:-server.log}"
WAIT_SECONDS="${WAIT_SECONDS:-40}"

if [[ -z "${TOKEN}" ]]; then
  echo "TOKEN is empty" >&2
  exit 1
fi

cat <<'EOF'
Note: start the server with FORCE_SECKILL_FAIL_COUNT=4 to force 3 retries then DLQ.
Example:
  FORCE_SECKILL_FAIL_COUNT=4 go run cmd/server/main.go > server.log 2>&1 &
EOF

url="${BASE_URL}/voucher-order/seckill/${VOUCHER_ID}"
echo "sending seckill request: ${url}"
curl -sS -X POST "${url}" \
  -H "authorization: ${TOKEN}" \
  -H "content-type: application/json" \
  -d '' | sed 's/$/\n/' || true

if [[ -f "${LOG_FILE}" ]]; then
  echo "waiting for DLQ log (up to ${WAIT_SECONDS}s)..."
  end=$((SECONDS + WAIT_SECONDS))
  while (( SECONDS < end )); do
    if rg -n "publish to dlq|consumeDLQ email" "${LOG_FILE}" >/dev/null 2>&1; then
      rg -n "publish to dlq|consumeDLQ email" "${LOG_FILE}" | tail -n 5
      break
    fi
    sleep 1
  done
else
  echo "log file not found: ${LOG_FILE}"
fi

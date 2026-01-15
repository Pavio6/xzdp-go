#!/usr/bin/env bash
set -euo pipefail

BASE_URL="${BASE_URL:-http://127.0.0.1:8081}"
VOUCHER_ID="${VOUCHER_ID:-12}"
STOCK="${STOCK:-100}"
TOKENS_FILE="${TOKENS_FILE:-tokens.csv}"
K6_SCRIPT="${K6_SCRIPT:-scripts/k6/seckill_retry.js}"

MAX_VUS="${MAX_VUS:-200}"
RAMP_WINDOW="${RAMP_WINDOW:-2s}"
STOP_DELAY="${STOP_DELAY:-0.3}"
DOWN_DURATION="${DOWN_DURATION:-20}"
RESET="${RESET:-1}"
MODE="${MODE:-direct_kafka}"
MESSAGE_COUNT="${MESSAGE_COUNT:-2}"

MYSQL_CONTAINER="${MYSQL_CONTAINER:-mysql-hmdp}"
REDIS_CONTAINER="${REDIS_CONTAINER:-redis-hmdp}"
KAFKA_CONTAINER="${KAFKA_CONTAINER:-kafka-hmdp}"
KAFKA_BROKER="${KAFKA_BROKER:-localhost:9092}"
REDIS_HOST="${REDIS_HOST:-127.0.0.1}"
REDIS_PORT="${REDIS_PORT:-6379}"
REDIS_DB="${REDIS_DB:-0}"

if ! command -v docker >/dev/null 2>&1; then
  echo "docker not found; this script requires Docker to stop/start MySQL" >&2
  exit 1
fi

if [[ "${TOKENS_FILE}" = /* ]]; then
  TOKENS_PATH="${TOKENS_FILE}"
else
  TOKENS_PATH="$(pwd)/${TOKENS_FILE}"
fi

if [[ ! -f "${TOKENS_PATH}" ]]; then
  echo "tokens file not found: ${TOKENS_PATH}" >&2
  exit 1
fi

if [[ "${RESET}" == "1" ]]; then
  ./scripts/reset_seckill.sh "${VOUCHER_ID}" "${STOCK}"
fi

docker start "${MYSQL_CONTAINER}" >/dev/null

cleanup() {
  docker start "${MYSQL_CONTAINER}" >/dev/null || true
}
trap cleanup EXIT

if [[ "${MODE}" == "direct_kafka" ]]; then
  token="$(head -n 1 "${TOKENS_PATH}" | cut -d',' -f1 | xargs)"
  if [[ -z "${token}" ]]; then
    echo "token not found in ${TOKENS_PATH}" >&2
    exit 1
  fi
  if command -v redis-cli >/dev/null 2>&1; then
    user_id="$(redis-cli -h "${REDIS_HOST}" -p "${REDIS_PORT}" -n "${REDIS_DB}" HGET "login:token:${token}" id | xargs)"
  else
    user_id="$(docker exec "${REDIS_CONTAINER}" redis-cli -n "${REDIS_DB}" HGET "login:token:${token}" id | tr -d '\r' | xargs)"
  fi
  if [[ -z "${user_id}" || "${user_id}" == "(nil)" ]]; then
    echo "user id not found in redis for token" >&2
    exit 1
  fi

  echo "stopping MySQL for ${DOWN_DURATION}s and producing ${MESSAGE_COUNT} message(s) to Kafka..."
  docker stop "${MYSQL_CONTAINER}" >/dev/null

  for _ in $(seq 1 "${MESSAGE_COUNT}"); do
    order_id="$(date +%s%N)"
    payload="{\"orderId\":${order_id},\"userId\":${user_id},\"voucherId\":${VOUCHER_ID},\"createdAt\":$(date +%s),\"retryCount\":0}"
    docker exec -i "${KAFKA_CONTAINER}" bash -lc \
      "printf '%s\n' '${payload}' | /opt/kafka/bin/kafka-console-producer.sh --bootstrap-server ${KAFKA_BROKER} --topic seckill-orders" >/dev/null
  done

  sleep "${DOWN_DURATION}"
  docker start "${MYSQL_CONTAINER}" >/dev/null
else
  echo "starting k6 and will stop MySQL after ${STOP_DELAY}s for ${DOWN_DURATION}s..."
  k6 run \
    -e BASE_URL="${BASE_URL}" \
    -e VOUCHER_ID="${VOUCHER_ID}" \
    -e TOKENS_FILE="${TOKENS_PATH}" \
    -e MAX_VUS="${MAX_VUS}" \
    -e RAMP_WINDOW="${RAMP_WINDOW}" \
    "${K6_SCRIPT}" &
  k6_pid=$!

  sleep "${STOP_DELAY}"
  if kill -0 "${k6_pid}" >/dev/null 2>&1; then
    docker stop "${MYSQL_CONTAINER}" >/dev/null
    sleep "${DOWN_DURATION}"
    docker start "${MYSQL_CONTAINER}" >/dev/null
  fi

  wait "${k6_pid}" || true
fi
echo "done. check server.log for publish to dlq"

#!/usr/bin/env bash
set -euo pipefail

TOPIC="${TOPIC:-seckill-orders}"
PAYLOAD_FILE="${PAYLOAD_FILE:-}"
PAYLOAD="${PAYLOAD:-}"

REDIS_HOST="${REDIS_HOST:-127.0.0.1}"
REDIS_PORT="${REDIS_PORT:-6379}"
REDIS_DB="${REDIS_DB:-0}"
REDIS_CONTAINER="${REDIS_CONTAINER:-redis-hmdp}"

KAFKA_CONTAINER="${KAFKA_CONTAINER:-kafka-hmdp}"
KAFKA_BROKER="${KAFKA_BROKER:-localhost:9092}"

if ! command -v docker >/dev/null 2>&1; then
  echo "docker not found; this script expects Docker to access mysql/kafka containers" >&2
  exit 1
fi

if [[ -n "${PAYLOAD_FILE}" ]]; then
  if [[ ! -f "${PAYLOAD_FILE}" ]]; then
    echo "payload file not found: ${PAYLOAD_FILE}" >&2
    exit 1
  fi
  PAYLOAD="$(cat "${PAYLOAD_FILE}")"
fi

if [[ -z "${PAYLOAD}" ]]; then
  echo "payload is empty; set PAYLOAD or PAYLOAD_FILE" >&2
  exit 1
fi

echo "replaying dlq message to topic=${TOPIC}"
docker exec -i "${KAFKA_CONTAINER}" bash -lc \
  "printf '%s\n' '${PAYLOAD}' | /opt/kafka/bin/kafka-console-producer.sh --bootstrap-server ${KAFKA_BROKER} --topic ${TOPIC}" >/dev/null
if ! command -v python3 >/dev/null 2>&1; then
  echo "python3 not found; cannot sync redis" >&2
  exit 1
fi
voucher_id="$(python3 -c 'import json,sys;print(json.loads(sys.argv[1]).get("voucherId",""))' "${PAYLOAD}")"
user_id="$(python3 -c 'import json,sys;print(json.loads(sys.argv[1]).get("userId",""))' "${PAYLOAD}")"
if [[ -z "${voucher_id}" || -z "${user_id}" ]]; then
  echo "missing voucherId/userId in payload; cannot sync redis" >&2
  exit 1
fi
stock_key="seckill:stock:vid:${voucher_id}"
order_key="order:vid:${voucher_id}"
if command -v redis-cli >/dev/null 2>&1; then
  redis-cli -h "${REDIS_HOST}" -p "${REDIS_PORT}" -n "${REDIS_DB}" DECR "${stock_key}" >/dev/null
  redis-cli -h "${REDIS_HOST}" -p "${REDIS_PORT}" -n "${REDIS_DB}" SADD "${order_key}" "${user_id}" >/dev/null
else
  docker exec "${REDIS_CONTAINER}" redis-cli -n "${REDIS_DB}" DECR "${stock_key}" >/dev/null
  docker exec "${REDIS_CONTAINER}" redis-cli -n "${REDIS_DB}" SADD "${order_key}" "${user_id}" >/dev/null
fi
echo "requeued one message and synced redis"

#!/usr/bin/env bash
set -euo pipefail

VOUCHER_ID="${1:-12}"
STOCK="${2:-100}"

REDIS_HOST="${REDIS_HOST:-127.0.0.1}"
REDIS_PORT="${REDIS_PORT:-6379}"
REDIS_DB="${REDIS_DB:-0}"

REDIS_CONTAINER="${REDIS_CONTAINER:-redis-hmdp}"
if command -v redis-cli >/dev/null 2>&1; then
  redis-cli -h "${REDIS_HOST}" -p "${REDIS_PORT}" -n "${REDIS_DB}" \
    SET "seckill:stock:vid:${VOUCHER_ID}" "${STOCK}" >/dev/null

  redis-cli -h "${REDIS_HOST}" -p "${REDIS_PORT}" -n "${REDIS_DB}" \
    DEL "order:vid:${VOUCHER_ID}" >/dev/null
else
  docker exec "${REDIS_CONTAINER}" redis-cli -n "${REDIS_DB}" \
    SET "seckill:stock:vid:${VOUCHER_ID}" "${STOCK}" >/dev/null
  docker exec "${REDIS_CONTAINER}" redis-cli -n "${REDIS_DB}" \
    DEL "order:vid:${VOUCHER_ID}" >/dev/null
fi

echo "reset seckill voucher=${VOUCHER_ID} stock=${STOCK} (redis ${REDIS_HOST}:${REDIS_PORT}/${REDIS_DB})"

MYSQL_HOST="${MYSQL_HOST:-127.0.0.1}"
MYSQL_PORT="${MYSQL_PORT:-3306}"
MYSQL_USER="${MYSQL_USER:-root}"
MYSQL_PASS="${MYSQL_PASS:-root}"
MYSQL_DB="${MYSQL_DB:-hmdp}"
MYSQL_CONTAINER="${MYSQL_CONTAINER:-mysql-hmdp}"

if command -v mysql >/dev/null 2>&1; then
  mysql -h "${MYSQL_HOST}" -P "${MYSQL_PORT}" -u "${MYSQL_USER}" -p"${MYSQL_PASS}" "${MYSQL_DB}" \
    -e "DELETE FROM tb_voucher_order; UPDATE tb_seckill_voucher SET stock=${STOCK} WHERE voucher_id=${VOUCHER_ID};"
else
  docker exec "${MYSQL_CONTAINER}" mysql -u "${MYSQL_USER}" -p"${MYSQL_PASS}" "${MYSQL_DB}" \
    -e "DELETE FROM tb_voucher_order; UPDATE tb_seckill_voucher SET stock=${STOCK} WHERE voucher_id=${VOUCHER_ID};"
fi

echo "reset db tb_voucher_order and tb_seckill_voucher (voucher=${VOUCHER_ID} stock=${STOCK})"

#!/usr/bin/env bash
set -euo pipefail

COUNT="${COUNT:-10000}"
PHONE_PREFIX="${PHONE_PREFIX:-138}"
USERS_FILE="${USERS_FILE:-hmdp_tb_user.csv}"
TOKENS_FILE="${TOKENS_FILE:-tokens.csv}"
REDIS_ADDR="${REDIS_ADDR:-127.0.0.1:6379}"
REDIS_DB="${REDIS_DB:-0}"

if ! [[ "${COUNT}" =~ ^[0-9]+$ ]] || (( COUNT <= 0 )); then
  echo "COUNT must be a positive integer" >&2
  exit 1
fi

if (( COUNT > 99999999 )); then
  echo "COUNT is too large for the default phone generator" >&2
  exit 1
fi

tmp_file="$(mktemp)"
trap 'rm -f "${tmp_file}"' EXIT

for ((i = 0; i < COUNT; i++)); do
  printf "%s%08d\n" "${PHONE_PREFIX}" "${i}" >>"${tmp_file}"
done

mv "${tmp_file}" "${USERS_FILE}"
trap - EXIT

go run cmd/gen_tokens/main.go \
  -in "${USERS_FILE}" \
  -out "${TOKENS_FILE}" \
  -redis "${REDIS_ADDR}" \
  -db "${REDIS_DB}"

echo "prepared ${COUNT} users in ${USERS_FILE}"
echo "prepared tokens in ${TOKENS_FILE} and Redis ${REDIS_ADDR}/${REDIS_DB}"

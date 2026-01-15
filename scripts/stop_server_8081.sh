#!/usr/bin/env bash
set -euo pipefail

PORT="${1:-8081}"

pids="$(lsof -t -i :"${PORT}" || true)"
if [[ -z "${pids}" ]]; then
  echo "no process listening on :${PORT}"
  exit 0
fi

echo "stopping processes on :${PORT}: ${pids}"
kill ${pids}

#!/usr/bin/env bash
# Starts the full UI end-to-end stack: two MySQL 8.4 containers, mock LLM, mock Target Agent, API, worker and the built web UI.
#   e2e/stack.sh up     build and start everything (idempotent), prints "READY"
#   e2e/stack.sh down   stop everything and remove the containers
set -euo pipefail
export PATH="$PATH:/usr/local/go/bin"
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
RUN="$ROOT/.e2e"
PLATFORM_PORT=3330
TARGET_PORT=3331
DB=atp_ui_e2e

stop_procs() {
  for pidfile in "$RUN"/*.pid; do
    [ -e "$pidfile" ] || continue
    kill "$(cat "$pidfile")" 2>/dev/null || true
    rm -f "$pidfile"
  done
}

if [ "${1:-}" = "down" ]; then
  stop_procs
  docker rm -f atp-e2e-platform atp-e2e-target >/dev/null 2>&1 || true
  exit 0
fi

mkdir -p "$RUN/bin"
stop_procs
cd "$ROOT"
for name in api worker mock-llm mock-target e2e-seed; do go build -o "$RUN/bin/$name" "./cmd/$name"; done
(cd web && npm run build >/dev/null)

start_mysql() { # name port
  docker rm -f "$1" >/dev/null 2>&1 || true
  docker run -d --name "$1" -e MYSQL_ROOT_PASSWORD=e2eroot -e TZ=UTC -p "$2:3306" mysql:8.4 \
    --default-time-zone=+00:00 --character-set-server=utf8mb4 --collation-server=utf8mb4_0900_ai_ci >/dev/null
}
wait_mysql() { # name
  for _ in $(seq 1 90); do
    docker exec "$1" mysqladmin ping -h127.0.0.1 -uroot -pe2eroot --silent 2>/dev/null && return 0
    sleep 2
  done
  echo "MySQL $1 did not start" >&2; exit 1
}
start_mysql atp-e2e-platform "$PLATFORM_PORT"
start_mysql atp-e2e-target "$TARGET_PORT"
wait_mysql atp-e2e-platform
wait_mysql atp-e2e-target
sleep 3

export E2E_PLATFORM_DSN="root:e2eroot@tcp(127.0.0.1:$PLATFORM_PORT)/?parseTime=true&multiStatements=true"
export E2E_PLATFORM_DB="$DB"
"$RUN/bin/e2e-seed" init

PLATFORM_DSN="root:e2eroot@tcp(127.0.0.1:$PLATFORM_PORT)/$DB?parseTime=true&loc=UTC&charset=utf8mb4"
KEYS="SECRET_ENCRYPTION_KEY_ID=k1 SECRET_ENCRYPTION_KEY_IDS=k1 ATP_ENCRYPTION_KEY_K1=ZGV2ZWxvcG1lbnQtYXRwLWtleS0wMDAwMDAwMDAwMDE="

launch() { # name, then env assignments and command
  local name="$1"; shift
  setsid env "$@" </dev/null >"$RUN/$name.log" 2>&1 &
  echo $! >"$RUN/$name.pid"
}
launch mock-llm HTTP_ADDR=:18081 MOCK_LLM_BEARER_TOKEN=mock-llm-token "$RUN/bin/mock-llm"
launch mock-target HTTP_ADDR=:18082 MOCK_TARGET_BEARER_TOKEN=mock-target-development-token MOCK_TARGET_INDEX_DELAY=0s "$RUN/bin/mock-target"
# shellcheck disable=SC2086
launch api HTTP_ADDR=:8080 PLATFORM_MYSQL_DSN="$PLATFORM_DSN" API_BEARER_TOKEN=e2e-api-token-00000000000000000001 \
  GENERATOR_CONFIG_SHA256=2b1d05198f3a9e000964b7eb03f3c0b8e42d145b94bbaee9541dd5831844a663 $KEYS "$RUN/bin/api"
# shellcheck disable=SC2086
launch worker PLATFORM_MYSQL_DSN="$PLATFORM_DSN" WORKER_OWNER=e2e-worker $KEYS \
  GENERATION_MODEL_PROVIDER=openai GENERATION_MODEL_BASE_URL=http://127.0.0.1:18081/v1 GENERATION_MODEL_API_TOKEN=mock-llm-token \
  GENERATION_MODEL_NAME=mock-generation-v1 GENERATION_MODEL_ALIAS=generation-primary \
  JUDGE_MODEL_NAME=mock-judge-b JUDGE_MODEL_ALIAS=judge-b \
  GENERATION_ALT_MODEL_NAME=mock-generation-v2 GENERATION_ALT_MODEL_ALIAS=generation-alt "$RUN/bin/worker"
(cd web && setsid npx vite preview --port 4173 --strictPort </dev/null >"$RUN/web.log" 2>&1 & echo $! >"$RUN/web.pid")

for url in http://127.0.0.1:8080/healthz http://127.0.0.1:18081/healthz http://127.0.0.1:18082/healthz http://127.0.0.1:4173/; do
  for _ in $(seq 1 60); do curl -fsS "$url" >/dev/null 2>&1 && break; sleep 1; done
  curl -fsS "$url" >/dev/null
done
echo READY

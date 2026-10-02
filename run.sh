#!/usr/bin/env bash
# Build and start the whole local auth demo, wait until healthy, open every UI.
#
#   ./run.sh          start (or rebuild) and open the UIs in the browser
#   ./run.sh test     start, then run the end-to-end checks, then open the UIs
#   ./run.sh down     stop and remove everything (Keycloak state is in-memory)
#   ./run.sh logs     follow logs of all services
set -euo pipefail
cd "$(dirname "$0")"

URLS=(
  "http://localhost:8080/admin/master/console/#/staff/events"   # Keycloak admin: staff realm events (admin / admin)
  "http://localhost:3000"                                  # Customer portal
  "http://localhost:3001"                                  # Backoffice portal
  "http://localhost:4000"                                  # Entra Origination console
)

case "${1:-up}" in
  down) docker compose down --remove-orphans; exit 0 ;;
  logs) exec docker compose logs -f ;;
  up|test) ;;
  *) echo "usage: $0 [up|test|down|logs]"; exit 2 ;;
esac

command -v docker >/dev/null || { echo "docker is required"; exit 1; }
docker info >/dev/null 2>&1 || { echo "Docker daemon is not running"; exit 1; }

for p in 8080 3000 3001 4000; do
  if lsof -nP -iTCP:$p -sTCP:LISTEN 2>/dev/null | grep -v com.docke | grep -q LISTEN; then
    echo "Port $p is used by another process (not this stack):"; lsof -nP -iTCP:$p -sTCP:LISTEN; exit 1
  fi
done

echo "==> Building and starting (Keycloak first boot takes ~20s)"
# Recreate so Keycloak re-imports the realm files on every run.
docker compose up -d --build --force-recreate --wait

wait_for() {
  local url=$1 name=$2
  for _ in $(seq 1 60); do
    curl -fsS -o /dev/null "$url" && { echo "    ok  $name"; return 0; }
    sleep 1
  done
  echo "    TIMEOUT waiting for $name ($url)"; docker compose logs --tail 40; exit 1
}
echo "==> Waiting for services"
wait_for http://localhost:8080/realms/staff/.well-known/openid-configuration "Keycloak (staff realm)"
wait_for http://localhost:8080/realms/customer/.well-known/openid-configuration "Keycloak (customer realm)"
wait_for http://localhost:4000/admin/state "Entra Origination API"
wait_for http://localhost:3000/ "Customer BFF"
wait_for http://localhost:3001/ "Backoffice BFF"

if [[ "${1:-up}" == "test" ]]; then
  echo "==> Running end-to-end checks"
  if command -v go >/dev/null; then
    go run ./cmd/e2e
  else
    docker run --rm --network host -v "$PWD":/src -w /src golang:1.25-alpine go run ./cmd/e2e
  fi
  # Tests leave rows in the in-memory tables; restart the API for a clean demo.
  docker compose restart origination-api >/dev/null && wait_for http://localhost:4000/admin/state "Entra Origination API (reset)"
fi

echo "==> Opening UIs"
opener=open; command -v open >/dev/null || opener=xdg-open
for u in "${URLS[@]}"; do "$opener" "$u" >/dev/null 2>&1 || echo "    open $u"; done

cat <<'TXT'

  Keycloak admin           http://localhost:8080/admin      admin / admin
  Customer portal          http://localhost:3000            BankID (mock): sven / test, lisa / test
  Backoffice portal        http://localhost:3001            Google (mock): anna.advisor, ulf.underwriter,
                                                            sara.support, lars.leaver  (password: test)
  Entra Origination        http://localhost:4000            people log + business tables + leaver check

  Impersonation: Customer portal -> "Sign in as staff to impersonate" -> sara.support
  Stop:          ./run.sh down
TXT

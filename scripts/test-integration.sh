#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
name="broto-test-$(date +%s)-$$"
cleanup() { docker rm -f "$name" >/dev/null 2>&1 || true; }
trap cleanup EXIT INT TERM
docker run --rm -d --name "$name" --memory=256m \
  --tmpfs /var/lib/postgresql/data:rw,size=192m \
  -e POSTGRES_PASSWORD=broto_test_only -e POSTGRES_DB=broto_test \
  -p 127.0.0.1::5432 postgres:17-alpine \
  -c shared_buffers=32MB -c max_wal_size=128MB -c min_wal_size=32MB >/dev/null
attempt=0
until docker exec "$name" pg_isready -h 127.0.0.1 -U postgres -d broto_test >/dev/null 2>&1; do
  attempt=$((attempt + 1))
  if [ "$attempt" -ge 30 ]; then docker logs "$name"; exit 1; fi
  sleep 1
done
port=$(docker inspect -f '{{(index (index .NetworkSettings.Ports "5432/tcp") 0).HostPort}}' "$name")
TEST_DATABASE_URL="postgres://postgres:broto_test_only@127.0.0.1:$port/broto_test?sslmode=disable" go test -race ./... -count=1 -timeout=180s "$@"

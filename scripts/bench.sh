#!/usr/bin/env bash
# End-to-end HTTP benchmark of Forge-Go (docs/BENCHMARKS.md).
#
# Builds the forge CLI, generates an application against this checkout
# (`forge new --framework-path`), scaffolds a tenant-scoped "post" resource,
# wires it, runs it against PostgreSQL as a non-superuser role (so row-level
# security applies, as in production) and measures it with the standard
# library load generator in internal/loadgen: requests per second and
# p50/p95/p99 latency of GET /health, the welcome page, the scaffold index
# (HTML), the JSON API show and the scaffold create (form POST with CSRF).
# It prints a markdown table.
#
#   export FORGE_TEST_POSTGRES_URL='postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable'
#   scripts/bench.sh                       # 16 workers, 10s per scenario, 3 runs
#   BENCH_DURATION=3s BENCH_RUNS=1 scripts/bench.sh
#   scripts/bench.sh -format json          # extra arguments go to the load generator
#
# FORGE_TEST_POSTGRES_URL names a server whose user may create roles and
# databases; the script creates a role and a database of its own and drops
# both, and deletes everything it built, when it ends.
#
# Settings (environment): BENCH_CONCURRENCY (16), BENCH_DURATION (10s),
# BENCH_WARMUP (2s), BENCH_RUNS (3), BENCH_ROWS (50), BENCH_PORT (18080).
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
admin="${FORGE_TEST_POSTGRES_URL:-}"
if [ -z "$admin" ]; then
	echo "bench.sh: set FORGE_TEST_POSTGRES_URL to a PostgreSQL server whose user may create roles and databases" >&2
	exit 2
fi
for tool in go psql openssl; do
	command -v "$tool" >/dev/null || { echo "bench.sh: $tool is required" >&2; exit 2; }
done

concurrency="${BENCH_CONCURRENCY:-16}"
duration="${BENCH_DURATION:-10s}"
warmup="${BENCH_WARMUP:-2s}"
runs="${BENCH_RUNS:-3}"
rows="${BENCH_ROWS:-50}"
port="${BENCH_PORT:-18080}"

work="$(mktemp -d "${TMPDIR:-/tmp}/forge-bench.XXXXXX")"
suffix="$(openssl rand -hex 4)"
role="forge_bench_${suffix}"
database="forge_bench_${suffix}"
role_password="$(openssl rand -hex 16)"
owner_password="$(openssl rand -hex 16)"
server_pid=""
created_database=0
created_role=0

cleanup() {
	status=$?
	trap - EXIT INT TERM
	if [ -n "$server_pid" ]; then
		kill "$server_pid" 2>/dev/null || true
		wait "$server_pid" 2>/dev/null || true
	fi
	if [ "$created_database" = 1 ]; then
		psql "$admin" -qAtc "DROP DATABASE IF EXISTS ${database} WITH (FORCE)" >/dev/null 2>&1 || echo "bench.sh: could not drop database ${database}" >&2
	fi
	if [ "$created_role" = 1 ]; then
		psql "$admin" -qAtc "DROP ROLE IF EXISTS ${role}" >/dev/null 2>&1 || echo "bench.sh: could not drop role ${role}" >&2
	fi
	if [ "$status" != 0 ] && [ -f "$work/server.log" ]; then
		echo "--- application log (last lines) ---" >&2
		tail -n 20 "$work/server.log" >&2 || true
	fi
	rm -rf "$work"
	exit "$status"
}
trap cleanup EXIT INT TERM

echo "building the forge CLI and the load generator" >&2
(cd "$root" && go build -o "$work/forge" ./cmd/forge && go build -o "$work/loadgen" ./internal/loadgen)

echo "generating the application" >&2
# forge new vendors the framework checkout; packages the scaffold adds later are not in vendor/.
export GOFLAGS=-mod=mod
app="$work/benchapp"
# --api-auth: loadgen signs in once through the bearer POST /auth/login.
"$work/forge" new benchapp --dir "$app" --framework-path "$root" --api-auth >/dev/null
(cd "$app" && "$work/forge" generate scaffold post title:string body:text --tenant-scoped >/dev/null)

# The generators never edit bootstrap.go; paste what the scaffold prints,
# once, ahead of the closing comment and return of identityRoutes.
bootstrap="$app/app/bootstrap/bootstrap.go"
marker='// Pages behind a permission authenticate the session cookie'
grep -qF "$marker" "$bootstrap" || { echo "bench.sh: bootstrap.go changed: marker $marker not found" >&2; exit 1; }
cat >"$work/wiring.go.txt" <<'GO'
	protect := func(permission string, next http.Handler) (http.Handler, error) {
		required, err := auth.NewPermission(permission)
		if err != nil {
			return nil, err
		}
		guarded, err := auth.RequireAudited(required, next, repository)
		if err != nil {
			return nil, err
		}
		return middleware.Authenticate(guarded), nil
	}
	protectCookie := func(permission string, next http.Handler) (http.Handler, error) {
		required, err := auth.NewPermission(permission)
		if err != nil {
			return nil, err
		}
		guarded, err := auth.RequireAudited(required, next, repository)
		if err != nil {
			return nil, err
		}
		return middleware.AuthenticateBrowser("/session/new", guarded)
	}
	postService, err := application.NewPostService(persistence.NewPostRepository(database), time.Now, uuid.New)
	if err != nil {
		return err
	}
	if err := httpapi.RegisterPosts(app, postService, protect); err != nil {
		return err
	}
	if err := htmlui.RegisterPosts(app, postService, renderer, protectCookie); err != nil {
		return err
	}
GO
cat >"$work/imports.go.txt" <<'GO'
	"time"

	"github.com/rocketinsidecorp/forge-go/uuid"
	"benchapp/app/adapters/htmlui"
	"benchapp/app/adapters/httpapi"
	"benchapp/app/adapters/persistence"
	"benchapp/app/application"
GO
awk -v marker="$marker" -v wiring="$work/wiring.go.txt" -v imports="$work/imports.go.txt" '
	/^import \($/ { print; while ((getline line < imports) > 0) print line; next }
	index($0, marker) { while ((getline line < wiring) > 0) print line }
	{ print }
' "$bootstrap" >"$bootstrap.new"
mv "$bootstrap.new" "$bootstrap"
(cd "$app" && gofmt -w app/bootstrap/bootstrap.go && go build -o "$work/benchapp-server" ./cmd/benchapp)

echo "creating the database ${database} with the non-superuser role ${role}" >&2
# The statement goes on stdin (printf is a builtin), so the role password never
# appears in psql's arguments, which ps shows.
printf "CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOBYPASSRLS CREATEDB;\n" "$role" "$role_password" |
	psql "$admin" -qAt -v ON_ERROR_STOP=1 >/dev/null
created_role=1
psql "$admin" -qAtc "CREATE DATABASE ${database} OWNER ${role}" >/dev/null
created_database=1
# Same host and port as the admin URL, the new role and database.
authority="$(printf '%s' "$admin" | sed -E 's#^[a-z]+://([^@/]*@)?([^/?]+).*$#\2#')"
export FORGE_DATABASE_URL="postgres://${role}:${role_password}@${authority}/${database}?sslmode=disable"
export FORGE_ENV=development
export FORGE_HTTP_ADDR="127.0.0.1:${port}"

"$work/benchapp-server" migrate >"$work/migrate.log" 2>&1 || { cat "$work/migrate.log" >&2; exit 1; }
FORGE_SETUP_PASSWORD="$owner_password" "$work/benchapp-server" setup --tenant bench --email bench@example.com --organization "Bench" >"$work/setup.log" 2>&1 || { cat "$work/setup.log" >&2; exit 1; }

echo "starting the application on 127.0.0.1:${port}" >&2
"$work/benchapp-server" >"$work/server.log" 2>&1 &
server_pid=$!

meta_cpu="unknown"
if [ -r /proc/cpuinfo ]; then
	meta_cpu="$(grep -m1 'model name' /proc/cpuinfo | sed 's/^[^:]*: *//')"
elif command -v sysctl >/dev/null; then
	meta_cpu="$(sysctl -n machdep.cpu.brand_string 2>/dev/null || echo unknown)"
fi
cpus="$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo '?')"
commit="$(git -C "$root" rev-parse --short HEAD 2>/dev/null || echo unknown)"
dirty=""
if [ -n "$(git -C "$root" status --porcelain 2>/dev/null)" ]; then dirty="+dirty"; fi
echo "Forge-Go ${commit}${dirty}, $(go version | cut -d' ' -f3-), $(uname -sr), ${cpus} x ${meta_cpu}"
echo "$(psql "$admin" -qAtc 'select version()' | cut -d, -f1), date $(date -u +%Y-%m-%dT%H:%MZ)"
echo

BENCH_PASSWORD="$owner_password" "$work/loadgen" \
	-base "http://127.0.0.1:${port}" -tenant bench -email bench@example.com \
	-concurrency "$concurrency" -duration "$duration" -warmup "$warmup" -runs "$runs" -rows "$rows" "$@"

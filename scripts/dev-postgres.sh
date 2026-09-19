#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
# SPDX-License-Identifier: Apache-2.0
#
# A throwaway Postgres for running the store suite against a second engine.
# SQLite stays the local default and needs none of this; the container exists
# so the Postgres code path can be exercised before it reaches a real database.
#
#   ./scripts/dev-postgres.sh up      start it and wait until it accepts queries
#   ./scripts/dev-postgres.sh down    stop it and drop its data
#   ./scripts/dev-postgres.sh status  report whether it is running; non-zero if not
#   ./scripts/dev-postgres.sh dsn     print the connection string
#
# Nothing here is used in production or by CI.

set -euo pipefail

PG_IMAGE="${PG_IMAGE:-docker.io/library/postgres:17}"
PG_CONTAINER="${PG_CONTAINER:-rabbithole-dev-postgres}"
PG_PORT="${PG_PORT:-5433}"
PG_USER="${PG_USER:-rabbithole}"
PG_PASSWORD="${PG_PASSWORD:-rabbithole}"
PG_DB="${PG_DB:-rabbithole_test}"

# 5433 by default so a Postgres already installed on the host keeps 5432.

runtime() {
	if command -v podman >/dev/null 2>&1; then
		echo podman
	elif command -v docker >/dev/null 2>&1; then
		echo docker
	else
		echo "need podman or docker on PATH" >&2
		exit 1
	fi
}

RUNTIME="$(runtime)"

dsn() {
	echo "postgres://${PG_USER}:${PG_PASSWORD}@127.0.0.1:${PG_PORT}/${PG_DB}?sslmode=disable"
}

running() {
	[ -n "$("$RUNTIME" ps --quiet --filter "name=^${PG_CONTAINER}$" 2>/dev/null)" ]
}

exists() {
	[ -n "$("$RUNTIME" ps --all --quiet --filter "name=^${PG_CONTAINER}$" 2>/dev/null)" ]
}

up() {
	if running; then
		echo "already running on port ${PG_PORT}"
		dsn
		return 0
	fi
	# A leftover stopped container would hold the name.
	if exists; then
		"$RUNTIME" rm --force "$PG_CONTAINER" >/dev/null
	fi

	echo "starting ${PG_IMAGE} as ${PG_CONTAINER} on port ${PG_PORT}"
	# No volume: the data is meant to die with the container. fsync off trades
	# crash safety, which a throwaway does not need, for a faster suite.
	"$RUNTIME" run --detach \
		--name "$PG_CONTAINER" \
		--env "POSTGRES_USER=${PG_USER}" \
		--env "POSTGRES_PASSWORD=${PG_PASSWORD}" \
		--env "POSTGRES_DB=${PG_DB}" \
		--publish "127.0.0.1:${PG_PORT}:5432" \
		"$PG_IMAGE" -c fsync=off -c full_page_writes=off >/dev/null

	printf 'waiting for postgres'
	for _ in $(seq 1 60); do
		if "$RUNTIME" exec "$PG_CONTAINER" pg_isready --quiet \
			--username "$PG_USER" --dbname "$PG_DB" >/dev/null 2>&1; then
			printf ' ready\n'
			dsn
			return 0
		fi
		printf '.'
		sleep 1
	done

	printf '\n'
	echo "postgres did not become ready; logs follow" >&2
	"$RUNTIME" logs --tail 30 "$PG_CONTAINER" >&2
	exit 1
}

down() {
	if ! exists; then
		echo "not running"
		return 0
	fi
	"$RUNTIME" rm --force --volumes "$PG_CONTAINER" >/dev/null
	echo "removed ${PG_CONTAINER}"
}

status() {
	if running; then
		echo "running on port ${PG_PORT}"
		dsn
		return 0
	fi
	if exists; then
		echo "stopped (run 'down' to clear it, then 'up')"
	else
		echo "not running"
	fi
	return 1
}

case "${1:-}" in
up) up ;;
down) down ;;
status) status ;;
dsn) dsn ;;
*)
	echo "usage: $0 {up|down|status|dsn}" >&2
	exit 2
	;;
esac

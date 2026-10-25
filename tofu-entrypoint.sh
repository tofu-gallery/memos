#!/bin/sh
# Tofu (2026-10-25): this file is Tofu's addition; it is not part of upstream Memos.
#
# Maps the variables Tofu sets on every app onto Memos' own MEMOS_* settings, then hands over to upstream's
# entrypoint (scripts/entrypoint.sh), which starts the server with the same arguments:
#   PORT          -> MEMOS_PORT. Tofu sends traffic to PORT, so PORT wins over the image's MEMOS_PORT (5230);
#                    without PORT, 5230 stays.
#   DATABASE_URL  -> MEMOS_DRIVER=postgres and MEMOS_DSN, with binary_parameters=yes added (see below). A MEMOS_DSN
#                    the owner sets is kept as it is, and so is a MEMOS_DRIVER.
# It also decides which user the server runs as and where its scratch files go.
set -eu

if [ -n "${PORT:-}" ]; then
	MEMOS_PORT=$PORT
	export MEMOS_PORT
fi

if [ -n "${DATABASE_URL:-}" ] && [ -z "${MEMOS_DSN:-}" ]; then
	case $DATABASE_URL in
	postgres://* | postgresql://*)
		# Memos talks to PostgreSQL through lib/pq, which sends a query that has parameters in two round trips
		# (prepare, then execute) unless binary_parameters=yes, which sends it in one. Tofu's DATABASE_URL goes
		# through a transaction pooler, which may hand the second round trip to another server connection.
		dsn=$DATABASE_URL
		case $dsn in
		*binary_parameters=*) ;;
		*\?*) dsn="$dsn&binary_parameters=yes" ;;
		*) dsn="$dsn?binary_parameters=yes" ;;
		esac
		: "${MEMOS_DRIVER:=postgres}"
		MEMOS_DSN=$dsn
		export MEMOS_DRIVER MEMOS_DSN
		;;
	*)
		echo "tofu-entrypoint: DATABASE_URL is not a postgres:// URL, so MEMOS_DRIVER and MEMOS_DSN are left as they are" >&2
		;;
	esac
fi

# Upstream's entrypoint drops from root to the nonroot user (MEMOS_UID:MEMOS_GID, 10001) with su-exec. Only root may
# listen on a port below 1024, and Tofu's PORT is 80 unless the app sets another, so the server stays root when its
# port is below 1024, and also when su-exec cannot switch users on this host. MEMOS_ENTRYPOINT_SWITCHED is
# upstream's own marker for "the switch already happened", which makes its entrypoint skip it.
if [ "$(id -u)" = "0" ]; then
	port=${MEMOS_PORT:-5230}
	if { [ "$port" -eq "$port" ] 2>/dev/null && [ "$port" -lt 1024 ]; } ||
		! su-exec "${MEMOS_UID:-10001}:${MEMOS_GID:-10001}" true >/dev/null 2>&1; then
		MEMOS_ENTRYPOINT_SWITCHED=1
		export MEMOS_ENTRYPOINT_SWITCHED
	fi
fi

# Memos keeps scratch files (uploads in progress) in its data directory, /var/opt/memos in this image. A server that
# does not run as root and cannot write there uses a folder under /tmp instead. Nothing there outlives a restart, and
# with DATABASE_URL set Memos keeps everything else, attachments included, in the database.
if [ -z "${MEMOS_DATA:-}" ] && [ "$(id -u)" != "0" ] && [ ! -w /var/opt/memos ]; then
	MEMOS_DATA=/tmp/memos
	mkdir -p "$MEMOS_DATA"
	export MEMOS_DATA
fi

exec "$@"

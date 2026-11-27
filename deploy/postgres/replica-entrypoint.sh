#!/bin/sh
# Streaming read replica. On first start, clone the primary with
# pg_basebackup (-R writes standby.signal and primary_conninfo), then run as
# a hot standby. REPLICA_APPLY_DELAY (e.g. 2s) artificially delays applying
# WAL so replication-lag bugs can be reproduced deterministically.
set -eu
if [ ! -s "$PGDATA/PG_VERSION" ]; then
  until pg_isready -h "$PRIMARY_HOST" -U "$POSTGRES_USER" >/dev/null 2>&1; do sleep 1; done
  PGPASSWORD="$POSTGRES_PASSWORD" pg_basebackup -h "$PRIMARY_HOST" -U "$POSTGRES_USER" \
    -D "$PGDATA" -R -X stream -c fast
  chown -R postgres:postgres "$PGDATA"
  chmod 700 "$PGDATA"
fi
# Extra arguments are passed through. Settings such as max_wal_senders and
# max_replication_slots must be at least the primary's, or recovery refuses
# to start ("insufficient parameter settings").
exec docker-entrypoint.sh postgres -c hot_standby=on \
  -c recovery_min_apply_delay="${REPLICA_APPLY_DELAY:-0}" -c hba_file=/etc/postgresql/pg_hba.conf "$@"

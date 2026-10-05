#!/bin/sh
# ClickHouse for spending analytics: data in ~/Library/Application Support/spendbot/clickhouse,
# listening on 127.0.0.1 only (HTTP 8123). Not exposed to the network.
set -eu
D="$HOME/Library/Application Support/spendbot/clickhouse"
mkdir -p "$D"
cd "$D"
exec /opt/homebrew/bin/clickhouse server -- \
  --path="$D/" --listen_host=127.0.0.1 --http_port=8123 --tcp_port=9000 \
  --mysql_port=0 --postgresql_port=0 --interserver_http_port=0 \
  --logger.console=0 --logger.log="$D/server.log" --logger.errorlog="$D/server.err.log" --logger.size=10M --logger.count=3

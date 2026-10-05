#!/bin/sh
# Runs spendbot on a Mac from launchd: reads deploy/.env; the database and backups live in the repo root.
# caffeinate -i keeps the Mac from idle sleep while the bot runs (closing the lid still sleeps it).
set -eu
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"
set -a
. ./deploy/.env
set +a
export DB_PATH="$ROOT/spend.db" BACKUP_DIR="$ROOT/backups"
# launchd provides a minimal PATH, and claude is installed via Homebrew
export PATH="/opt/homebrew/bin:/usr/local/bin:$PATH"
exec /usr/bin/caffeinate -i "$ROOT/bin/spendbot"

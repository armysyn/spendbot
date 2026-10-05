.PHONY: build test lint run docker dist mac-install mac-uninstall mac-restart mac-logs

build:
	CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o bin/spendbot ./cmd/spendbot

test:
	go test -race ./...

lint:
	go vet ./...
	golangci-lint run

run:
	set -a && . deploy/.env && set +a && DB_PATH=./spend.db BACKUP_DIR=./backups go run ./cmd/spendbot

docker:
	docker compose -f deploy/compose.yaml up -d --build

# Archives for Windows, macOS and Linux in dist/
dist:
	./scripts/dist.sh

# Run permanently on a Mac via launchd: start at login, restart on crash.
# Ollama runs separately: brew services start ollama.
LA := $(HOME)/Library/LaunchAgents
GUI := gui/$$(id -u)

mac-install: build
	sed 's|@ROOT@|$(CURDIR)|g' deploy/launchd/clickhouse.plist.in > $(LA)/local.spendbot.clickhouse.plist
	sed 's|@ROOT@|$(CURDIR)|g' deploy/launchd/spendbot.plist.in > $(LA)/local.spendbot.plist
	launchctl bootout $(GUI)/local.spendbot.clickhouse 2>/dev/null || true
	launchctl bootout $(GUI)/local.spendbot 2>/dev/null || true
	launchctl bootstrap $(GUI) $(LA)/local.spendbot.clickhouse.plist
	launchctl bootstrap $(GUI) $(LA)/local.spendbot.plist

mac-restart: build
	launchctl kickstart -k $(GUI)/local.spendbot

mac-uninstall:
	launchctl bootout $(GUI)/local.spendbot 2>/dev/null || true
	launchctl bootout $(GUI)/local.spendbot.clickhouse 2>/dev/null || true
	rm -f $(LA)/local.spendbot.plist $(LA)/local.spendbot.clickhouse.plist

mac-logs:
	tail -f spendbot.log

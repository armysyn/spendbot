# spendbot

Personal spending tracker for Kaspi Gold statements and Apple Wallet payments. One Go binary
with no dependencies: pure-Go SQLite, a web page, and an optional Telegram bot, ClickHouse and a
local model through Ollama. Your data never leaves your computer unless you connect a cloud model.

**Features**

- **Kaspi Gold PDF statements** — parsed by text coordinates and reconciled with the statement
  header totals to the tiyn; re-importing or overlapping statements never duplicates operations.
- **Batched questions** — the local model guesses the category of each new merchant, questions
  pile up in batches, and a whole batch is answered at once; answered merchants are categorized automatically.
- **Home dashboard** — this month so far against the same days of the last one, the month's pace,
  day-by-day chart, top categories, recent operations and insights.
- **Analytics** — spending by day or week, month, weekday (calendar average), category and
  merchant, a calendar heatmap, most expensive days, check sizes; changes against the previous
  period; a "without savings" switch; reconciliation with every statement. Every bar, day and
  merchant opens its operations, and their sum equals the number on the chart.
- **Operations** — every operation, with filters by period, kind, category, merchant or person,
  text, amount range and direction; sorting by date or amount; grouping by month, week, day,
  weekday, category, merchant or kind with sums; average, median and largest; CSV export; a
  category change for one purchase or all purchases of a merchant.
- **Income** — salaries stated over the years (current and past, per employer); salary arrivals
  found by themselves, since Kaspi marks them "Зарплата"; other income sources picked from regular
  top-ups; month by month: income, spending, money to and from people, what was left or the
  deficit, and a running total. Own money moved between banks and lenders do not count.
- **Transfers** — people money went to or came from, with totals, counts and balance per
  person over any period; filters by direction, total, number of transfers and people new in
  the period; sorting by total, count, balance or date.
- **Requests in plain words** — "between 20k and 50k, largest first" on the operations page,
  "sent only once between 20k and 50k" or "who sent me the most" on the transfers page,
  "what grew the most this year" on the categories page,
  become filters on top of the current ones. The local model (or the bot's model) only
  translates the words; it never sees the operations, and the program computes every number.
  Amounts and sorting words work without any model.
- **Categories** — per category over any period: spending, share, operations, average, change
  against the previous period, a sparkline and top merchants; filters by name, spending range and
  savings, unused ones on request, sorting by size, growth, fall, count, average or name;
  add, rename, merge, archive and mark as savings.
- **Background insights** — subscriptions and recurring payments, price increases, anomalies,
  double charges, growing categories, saving tips.
- **Transfers** — search by recipient name, sent and received per person; transfers to a
  person can count as spending in a category (rent).
- **Issues** — problems and wishes about spendbot written down on the site, with priority,
  the page they are about, comments, closing and search; kept locally for now.
- **Apple Wallet + Telegram** — the Shortcuts "Transaction" automation sends each payment and
  the bot asks "what for?" with category buttons.

Code does all the math. The model only guesses categories and writes tips from finished numbers.

## Quick start: prebuilt archive

`make dist` builds archives for Windows, macOS and Linux into `dist/`: the binary, a
single-computer `spendbot.env` and a step-by-step `README.txt`.

1. Unzip and run `start.bat` (Windows; offers to install Ollama via winget), `start.command`
   (macOS) or `./spendbot` (Linux). The page opens in a browser.
2. Settings → pick a model for your computer and download it (qwen2.5 7b / 3b / 1.5b).
3. Upload a PDF statement on the home page, answer question batches, open Analytics.

Without Telegram and ClickHouse everything works on the page and analytics uses SQLite.

## Settings

Read from `spendbot.env` next to the binary (or the path in `SPENDBOT_CONFIG`); environment
variables take precedence. Templates: `deploy/dist/spendbot.env` (one computer) and
`deploy/.env.example` (server or Mac with all parts).

| Variable | Default | |
| --- | --- | --- |
| `DATA_DIR`, `DB_PATH` | `data/` next to the binary | database and backups |
| `LISTEN_ADDR`, `PUBLIC_URL` | `:8080`, `http://localhost:8080` | page address and links in Telegram |
| `WEB_PASSWORD` | empty — no password | needed when the page is open to the network |
| `TELEGRAM_TOKEN`, `TELEGRAM_ALLOWED_CHAT_ID` | empty — no bot | the bot answers only this chat |
| `INGEST_TOKEN` | generated on first start | token for the iPhone automation, shown on Settings |
| `ANALYSIS_LLM_URL`, `ANALYSIS_LLM_MODEL` | Ollama on localhost, `qwen2.5:7b` | local model; `ANALYSIS_LLM=off` disables it |
| `LLM` | `off` | bot model: `claude` (Claude Code CLI) or `openai` (any compatible API) |
| `CLICKHOUSE_URL` | empty | when set, operations sync to ClickHouse and analytics reads from it |
| `QUIET_HOURS`, `WEEKLY_REPORT`, `EVENING_REPORT` | `23:00-08:00`, `MON 10:00`, `21:00` | bot questions and summaries |
| `OPEN_BROWSER`, `LOG_FORMAT` | `0`, `json` | for desktop use: `1`, `text` |

## Full setup on a Mac

```bash
cp deploy/.env.example deploy/.env      # Telegram, ClickHouse, models
brew install ollama && brew services start ollama
brew install --cask clickhouse          # optional
make mac-install                        # bot and ClickHouse as launchd services
make mac-restart                        # after code or deploy/.env changes
make mac-logs
```

On a server: `cd deploy && docker compose up -d --build` (distroless image).

### Telegram

Get a token from @BotFather. For the `chat_id`, message the bot and open
`https://api.telegram.org/bot<TOKEN>/getUpdates` while spendbot is not running. The bot offers
category buttons with the guess first, free-text replies (`groceries, gift for mom 2000`), manual
entries (`taxi 1200`), `/pending`, `/stat`, `/cat`, `/undo`, `/export`, quiet hours, batching,
reminders and summaries.

### iPhone

Shortcuts → Automation → Transaction, all cards, Run Immediately. Action "Get Contents of URL":
`POST <PUBLIC_URL>/api/v1/tx`, header `Authorization: Bearer <INGEST_TOKEN>`, JSON with `amount`,
`merchant`, `card`, `at` (ISO 8601). The phone must reach the computer (same Wi-Fi or Tailscale).

## How it counts

- **Spending** is a purchase (a refund is negative in the merchant's category), a cash withdrawal,
  or a transfer to a person marked as spending. Skipped operations, transfers to people and top-ups are not spending.
- **Reconciliation**: per operation kind, the sum over the statement period equals the header total.
- **Self-check**: sums by month, weekday and category equal the period total, or the page warns.
- **The source does not change the numbers**: analytics and insights are computed in Go over the
  same rows from SQLite or ClickHouse; a test compares the page from both sources field by field.
- Kaspi prints statements in Russian; those labels live in `internal/kaspi` as data.

## Layout

| Package | Purpose |
| --- | --- |
| `cmd/spendbot` | wiring, startup, `spendbot healthcheck`, `spendbot version` |
| `internal/config` | `spendbot.env` and environment, all fields validated at once |
| `internal/store` | SQLite, embedded migrations, `updated_at` triggers |
| `internal/kaspi`, `statement`, `importer` | Kaspi labels, PDF parsing, duplicate-free import |
| `internal/analytics` | analytics page and reconciliation — pure functions over operation rows |
| `internal/analysis` | merchant questions, batches, insights, ClickHouse sync |
| `internal/web` | home, operations, analytics, income, categories, transfers, issues, batches, settings, import pages |
| `internal/ingest` | `POST /api/v1/tx`, `GET /healthz`, token, rate limit, dedupe |
| `internal/bot`, `classify`, `llm` | Telegram, classification (memory → model → question), Ollama / Claude / OpenAI |
| `internal/money`, `merchant`, `report`, `scheduler` | amounts, merchant names, summaries, `VACUUM INTO` backups |

## Development

```bash
make test lint                                              # go test -race, golangci-lint
CLICKHOUSE_TEST_URL=http://localhost:8123 go test ./...     # plus ClickHouse tests
make dist                                                   # archives for all platforms
```

Real statements for tests go to `statements/` (not committed): a test parses each one and
checks it against its header totals.

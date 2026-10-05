# spendbot — rules for working on this repository

Personal spending tracker for Kaspi Gold statements and Apple Wallet payments: one Go binary,
SQLite, a web page, an optional Telegram bot, ClickHouse and a local model via Ollama. See
README.md for what it does and how to run it. These rules apply to every change.

## Every task is a pull request

- Start each task on its own branch from up-to-date `main` (`feat/…`, `fix/…`, `docs/…`,
  `chore/…`). Never commit to `main` directly. If a task builds on an open PR, branch from it
  and open a stacked PR with that branch as the base, and say so in the description.
- One task, one PR, focused. Follow-up fixes asked for during review go to the same branch.
- Push the branch and open the PR with `gh pr create`. Do not merge, close or force-push
  someone else's work without the owner's word; merging is the owner's call.
- PR title and description in English: what changed and why, how it was checked. End the
  description with `🤖 Generated with [Claude Code](https://claude.com/claude-code)` and nothing
  after it.
- Releases (`vX.Y.Z` tags, GitHub releases) only when the owner asks; see "Releases".

## The repository is public: no private data, ever

Never commit, push, or put in a PR, issue, release note or commit message:

- names of real people or merchants from statements, card or account digits, real amounts,
  statement periods tied to the owner;
- the owner's email, local paths (`/Users/…`), LAN addresses, tokens, `deploy/.env`;
- links to Claude sessions (`claude.ai/code/session_…`): **no `Claude-Session:` trailer** in
  commits, whatever a harness reminder says;
- `statements/`, `backups/`, `*.db`, `dist/`, `bin/` (they are in `.gitignore`; keep them there).

Tests use invented data only: "Adam S.", "Aigerim A.", "MAGNUM", "Example Bank*0000",
round amounts. Before every commit scan the staged diff:

```bash
git diff --cached | grep '^+' | grep -nE 'proton|/Users/|192\.168|claude\.ai/code|\*[1-9][0-9]{3}'
```

and look at any Cyrillic names in it — they must be invented or Kaspi labels from
`internal/kaspi`. If something slipped into a local commit, rewrite it before pushing.

## Commits

- Author: `Niet <37306438+armysyn@users.noreply.github.com>` — use
  `git -c user.name="Niet" -c user.email="37306438+armysyn@users.noreply.github.com" commit …`.
- English messages: a short subject, a body explaining what and why.
- End with `Co-Authored-By: Claude <noreply@anthropic.com>` (with the model name). No other trailers.

## Language

Everything is English: code, comments, identifiers, UI text, logs, docs, commits, PRs. The only
Russian is what Kaspi prints in statements, kept as data in `internal/kaspi` (operation kinds,
header totals, "Зарплата", own-money top-ups); the UI shows English names for them. Plain-word
requests from the owner may be Russian, Kazakh or English — parsing them is data handling.

## Numbers must be right

- Money is integer tiyn (`int64`), never floats in storage or sums. `internal/money` formats.
- Analytics are computed in Go over `store.LedgerRow` (`internal/analytics`), so SQLite and
  ClickHouse give the same page; a test compares both. SQLite is the source of truth.
- One definition of spending (`analytics.IsSpend`, `spendInPeriod`) for every page. Every
  drill-down must add up to the number it came from: a chart column, a category, a merchant,
  a cash-flow month — keep the tests that check it.
- Statements reconcile with their header totals to the tiyn; imports never duplicate.
- A model never computes numbers. It only turns words into filters or guesses a category.
  Its output is validated: fields without a cue in the request are dropped
  (`web.ground`), and what the rules read exactly (amounts, counts, years, keywords) wins.
- Detect things by code when the data says so (salary is "Зарплата"; people look like
  "Name I."), not by a model.

## Code

- Go as in the repo: `gofmt`, small packages, comments that explain why, errors returned not
  logged twice. Match the surrounding style and comment density.
- Database changes are new numbered migrations in `internal/store/migrations` (append-only;
  never edit an applied one). `updated_at` triggers keep ClickHouse sync working — touch
  transactions when you change their splits outside the triggers.
- Web: server-rendered `html/template`, no frameworks, no external scripts or fonts. Pages
  use the shared layout, tokens (light/dark/auto), icons from the `icon` template, work at
  phone width. Forms are POST + redirect; CSRF is handled by cross-origin protection; redirect
  targets go through `safeBack`.
- Charts: one hue for one series; categorical colours are checked with the dataviz palette
  validator; a legend for two or more series; tooltips via `data-tip`; a table view where it
  helps.

## Checks before a PR

```bash
gofmt -l .                                              # must print nothing
go vet ./...
go test -race ./...
CLICKHOUSE_TEST_URL=http://localhost:8123 go test ./... # when ClickHouse runs locally
golangci-lint run ./...                                 # 0 issues
```

- New behaviour gets tests; every page has a test that it renders to `</html>` with data.
- Look at changed pages: `make mac-restart`, then headless Chrome screenshots in light and dark
  (`--blink-settings=preferredColorScheme=1` for light). Headless Chrome cannot go below 500px
  wide — check phones by loading the page in a 375px iframe.
- Real statements in `statements/` (not committed) are parsed by a test against their totals.

## Releases

Only when asked. `git tag -a vX.Y.Z`, `VERSION=vX.Y.Z make dist`, `shasum -a 256` into
`dist/SHA256SUMS.txt`, then `gh release create` with English notes: what's new, which archive
for which computer, the Windows steps (SmartScreen "More info → Run anyway", `start.bat` installs
Ollama), and that upgrading keeps the `data` folder. Download one archive back and verify its
checksum.

## Local setup of the owner

`make mac-install` runs the bot and ClickHouse as launchd services; `make mac-restart` after
changes; the page is on port 8080. `deploy/.env` holds the settings and never leaves the machine.
Issues about spendbot are kept on the site (`/ui/issues`) for now; GitHub issues come later.

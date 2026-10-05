-- Imported statements with header totals, for reconciliation on the analytics page.
CREATE TABLE statements (
  id          INTEGER PRIMARY KEY,
  account     TEXT NOT NULL,
  period_from TEXT NOT NULL,
  period_to   TEXT NOT NULL,
  summary     TEXT NOT NULL, -- JSON: {header label: total in tiyn, signed as in the statement}
  ops         INTEGER NOT NULL,
  imported_at TEXT NOT NULL,
  UNIQUE (account, period_from, period_to)
);

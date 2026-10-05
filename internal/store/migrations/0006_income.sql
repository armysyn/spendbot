-- Salary as the person states it: a monthly amount over a span of months. An open end
-- (to_month NULL) is the current salary. Months are 'YYYY-MM'.
CREATE TABLE salary_periods (
  id         INTEGER PRIMARY KEY,
  from_month TEXT NOT NULL,
  to_month   TEXT,
  amount_minor INTEGER NOT NULL,
  employer   TEXT,
  note       TEXT
);

-- Senders of top-ups counted as income besides salary (a client, rent from a tenant).
-- name is the sender as printed in the statement.
CREATE TABLE income_sources (
  name       TEXT PRIMARY KEY,
  created_at TEXT NOT NULL
);

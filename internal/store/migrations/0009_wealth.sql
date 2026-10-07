-- The card balance at the start and the end of a statement, as Kaspi prints it ("available").
ALTER TABLE statements ADD COLUMN opening_minor INTEGER;
ALTER TABLE statements ADD COLUMN closing_minor INTEGER;

-- Categories whose spending repays debts: loan instalments, a mortgage.
ALTER TABLE categories ADD COLUMN debt INTEGER NOT NULL DEFAULT 0;

-- What a person owns and owes, with values on dates. Amounts are positive: a debt of
-- 1,000,000 ₸ is 1,000,000 with a debt kind.
CREATE TABLE holdings (
  id         INTEGER PRIMARY KEY,
  name       TEXT NOT NULL,
  kind       TEXT NOT NULL, -- cash | deposit | investment | property | vehicle | asset | loan | mortgage | credit | debt
  liquid     INTEGER NOT NULL DEFAULT 0, -- can be spent within days: counts for the cushion
  note       TEXT,
  archived   INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL
);
CREATE TABLE holding_values (
  id           INTEGER PRIMARY KEY,
  holding_id   INTEGER NOT NULL REFERENCES holdings(id) ON DELETE CASCADE,
  on_date      TEXT NOT NULL, -- YYYY-MM-DD
  amount_minor INTEGER NOT NULL,
  UNIQUE (holding_id, on_date)
);

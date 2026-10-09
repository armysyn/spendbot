-- Deleted operations, kept whole so they can be restored. Statement operations remember their
-- keys: importing the statement again does not bring them back, and the statement still
-- reconciles. A Wallet payment sent again with the same key is not added either.
CREATE TABLE trash (
  id            INTEGER PRIMARY KEY,
  tx_id         INTEGER NOT NULL, -- the id it had
  external_key  TEXT,
  occurred_at   TEXT NOT NULL,
  amount_minor  INTEGER NOT NULL,
  currency      TEXT NOT NULL,
  amount_raw    TEXT,
  merchant_raw  TEXT,
  merchant_norm TEXT,
  card          TEXT,
  source        TEXT NOT NULL,
  status        TEXT NOT NULL,
  note          TEXT,
  kind          TEXT,
  created_at    TEXT NOT NULL,
  deleted_at    TEXT NOT NULL
);
CREATE INDEX trash_external_key ON trash(external_key);
CREATE INDEX trash_occurred_at ON trash(occurred_at);

CREATE TABLE trash_links (
  op_key   TEXT PRIMARY KEY,
  trash_id INTEGER NOT NULL REFERENCES trash(id) ON DELETE CASCADE
);
CREATE INDEX trash_links_trash_id ON trash_links(trash_id);

CREATE TABLE trash_splits (
  id           INTEGER PRIMARY KEY,
  trash_id     INTEGER NOT NULL REFERENCES trash(id) ON DELETE CASCADE,
  category_id  INTEGER NOT NULL REFERENCES categories(id),
  amount_minor INTEGER NOT NULL,
  note         TEXT
);
CREATE INDEX trash_splits_trash_id ON trash_splits(trash_id);

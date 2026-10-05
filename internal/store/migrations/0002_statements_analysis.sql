-- Operation kind from the statement, as Kaspi prints it (see internal/kaspi).
ALTER TABLE transactions ADD COLUMN kind TEXT;
-- Last change of a transaction or its parts; ClickHouse sync follows it.
ALTER TABLE transactions ADD COLUMN updated_at TEXT;
UPDATE transactions SET updated_at = created_at;
CREATE INDEX transactions_updated_at ON transactions(updated_at);

-- New statuses: review — a statement purchase waiting for an answer in a question batch;
-- info — not spending (incoming money, transfers), kept for analysis.

-- Triggers maintain updated_at so sync does not depend on which code changed the data.
CREATE TRIGGER transactions_touch AFTER UPDATE ON transactions
WHEN NEW.updated_at IS OLD.updated_at
BEGIN
  UPDATE transactions SET updated_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now') WHERE id = NEW.id;
END;
CREATE TRIGGER transactions_touch_insert AFTER INSERT ON transactions
WHEN NEW.updated_at IS NULL
BEGIN
  UPDATE transactions SET updated_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now') WHERE id = NEW.id;
END;
CREATE TRIGGER splits_touch_insert AFTER INSERT ON splits BEGIN
  UPDATE transactions SET updated_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now') WHERE id = NEW.tx_id;
END;
CREATE TRIGGER splits_touch_delete AFTER DELETE ON splits BEGIN
  UPDATE transactions SET updated_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now') WHERE id = OLD.tx_id;
END;
CREATE TRIGGER categories_touch AFTER UPDATE OF name ON categories BEGIN
  UPDATE transactions SET updated_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
  WHERE id IN (SELECT tx_id FROM splits WHERE category_id = NEW.id);
END;

-- Deleted transactions, so they get deleted in ClickHouse too.
CREATE TABLE deleted_txs (tx_id INTEGER PRIMARY KEY, deleted_at TEXT NOT NULL);
CREATE TRIGGER transactions_deleted AFTER DELETE ON transactions BEGIN
  INSERT OR REPLACE INTO deleted_txs (tx_id, deleted_at) VALUES (OLD.id, strftime('%Y-%m-%dT%H:%M:%SZ', 'now'));
END;

-- Statement operation → transaction. Re-importing a statement duplicates nothing, and a
-- purchase that already came from Wallet is linked instead of being added twice.
CREATE TABLE statement_links (
  op_key TEXT PRIMARY KEY,
  tx_id  INTEGER NOT NULL REFERENCES transactions(id) ON DELETE CASCADE
);
CREATE INDEX statement_links_tx_id ON statement_links(tx_id);

-- Batches of merchant questions. They pile up until answered.
CREATE TABLE question_batches (
  id INTEGER PRIMARY KEY,
  created_at  TEXT NOT NULL,
  notified_at TEXT,
  answered_at TEXT
);

-- "What is this merchant" question — one per normalized name.
CREATE TABLE merchant_questions (
  merchant_norm     TEXT PRIMARY KEY,
  display           TEXT NOT NULL,
  guess_category_id INTEGER REFERENCES categories(id),
  batch_id          INTEGER REFERENCES question_batches(id),
  status            TEXT NOT NULL DEFAULT 'open', -- open | answered | ignored
  answer            TEXT,
  created_at        TEXT NOT NULL,
  answered_at       TEXT
);
CREATE INDEX merchant_questions_batch ON merchant_questions(batch_id);

-- Background analysis insights. key prevents repeating the same insight.
CREATE TABLE insights (
  id          INTEGER PRIMARY KEY,
  key         TEXT UNIQUE NOT NULL,
  kind        TEXT NOT NULL, -- recurring | price_up | anomaly | duplicate | trend | tip
  title       TEXT NOT NULL,
  body        TEXT NOT NULL,
  created_at  TEXT NOT NULL,
  notified_at TEXT,
  dismissed   INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE transactions (
  id            INTEGER PRIMARY KEY,
  external_key  TEXT UNIQUE,          -- guards against duplicates on resend
  occurred_at   TEXT NOT NULL,        -- RFC3339, UTC
  amount_minor  INTEGER NOT NULL,     -- 450000 = 4,500.00 ₸; 0 = amount not parsed
  currency      TEXT NOT NULL DEFAULT 'KZT',
  amount_raw    TEXT,                 -- as received from Wallet
  merchant_raw  TEXT,
  merchant_norm TEXT,
  card          TEXT,
  source        TEXT NOT NULL,        -- wallet | manual | import
  status        TEXT NOT NULL,        -- pending | asked | done | ignored
  note          TEXT,
  created_at    TEXT NOT NULL
);
CREATE INDEX transactions_occurred_at ON transactions(occurred_at);
CREATE INDEX transactions_status ON transactions(status);

CREATE TABLE categories (
  id INTEGER PRIMARY KEY, name TEXT UNIQUE NOT NULL, archived INTEGER NOT NULL DEFAULT 0
);

-- one transaction can be split: "groceries + gift 2000"
CREATE TABLE splits (
  id INTEGER PRIMARY KEY,
  tx_id INTEGER NOT NULL REFERENCES transactions(id) ON DELETE CASCADE,
  category_id INTEGER NOT NULL REFERENCES categories(id),
  amount_minor INTEGER NOT NULL,
  note TEXT
);
CREATE INDEX splits_tx_id ON splits(tx_id);

-- merchant memory: the more hits, the more confident the automatic category
CREATE TABLE merchant_rules (
  merchant_norm TEXT PRIMARY KEY,
  category_id INTEGER NOT NULL REFERENCES categories(id),
  hits INTEGER NOT NULL DEFAULT 1,
  updated_at TEXT NOT NULL
);

CREATE TABLE questions (
  tx_id INTEGER PRIMARY KEY REFERENCES transactions(id) ON DELETE CASCADE,
  tg_message_id INTEGER, asked_at TEXT, reminded_at TEXT
);
CREATE INDEX questions_tg_message_id ON questions(tg_message_id);

-- housekeeping values: when a report was last sent, a backup made, etc.
CREATE TABLE kv (key TEXT PRIMARY KEY, value TEXT NOT NULL);

INSERT INTO categories (name) VALUES
  ('Groceries'), ('Cafes & restaurants'), ('Transport'), ('Taxi'), ('Home'),
  ('Health'), ('Clothing'), ('Entertainment'), ('Gifts'), ('Phone & subscriptions'),
  ('Education'), ('Beauty'), ('Other');

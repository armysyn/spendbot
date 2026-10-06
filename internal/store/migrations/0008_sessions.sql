-- Signed-in browsers. id is the SHA-256 of the cookie token: a copy of the database does
-- not give working cookies. The page password hash lives in kv under 'web_password'.
CREATE TABLE sessions (
  id         TEXT PRIMARY KEY,
  created_at TEXT NOT NULL,
  seen_at    TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  agent      TEXT
);
CREATE INDEX sessions_expires ON sessions(expires_at);

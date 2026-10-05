-- Problems and wishes about spendbot itself, kept locally until GitHub issues are set up.
-- github_number links an issue once it is copied to GitHub.
CREATE TABLE issues (
  id            INTEGER PRIMARY KEY,
  kind          TEXT NOT NULL,             -- bug | idea
  title         TEXT NOT NULL,
  body          TEXT NOT NULL DEFAULT '',
  status        TEXT NOT NULL DEFAULT 'open', -- open | closed
  priority      TEXT NOT NULL DEFAULT 'normal', -- low | normal | high
  page          TEXT,                      -- the page it is about, e.g. /ui/income
  github_number INTEGER,
  created_at    TEXT NOT NULL,
  updated_at    TEXT NOT NULL,
  closed_at     TEXT
);
CREATE INDEX issues_status ON issues(status);

CREATE TABLE issue_comments (
  id         INTEGER PRIMARY KEY,
  issue_id   INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
  body       TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX issue_comments_issue ON issue_comments(issue_id);

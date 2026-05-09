CREATE TABLE rep_state (
  identity_id    BLOB    NOT NULL PRIMARY KEY CHECK(length(identity_id) = 32),
  state          INTEGER NOT NULL,
  since          INTEGER NOT NULL,
  first_seen_at  INTEGER NOT NULL,
  reasons        TEXT    NOT NULL DEFAULT '[]',
  updated_at     INTEGER NOT NULL
);
CREATE TABLE rep_events (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  identity_id  BLOB    NOT NULL CHECK(length(identity_id) = 32),
  role         INTEGER NOT NULL,
  event_type   INTEGER NOT NULL,
  value        REAL    NOT NULL,
  observed_at  INTEGER NOT NULL
);
CREATE INDEX idx_rep_events_id_time   ON rep_events(identity_id, observed_at);
CREATE INDEX idx_rep_events_type_time ON rep_events(event_type, observed_at);
CREATE TABLE rep_scores (
  identity_id  BLOB    NOT NULL PRIMARY KEY CHECK(length(identity_id) = 32),
  score        REAL    NOT NULL,
  updated_at   INTEGER NOT NULL
);

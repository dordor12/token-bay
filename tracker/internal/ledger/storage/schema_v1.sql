-- v1 schema for tracker/internal/ledger/storage.
-- Spec: docs/superpowers/specs/ledger/2026-04-22-ledger-design.md §5.2.
-- Migrations are append-only: a v2 schema lands as a separate set of
-- statements, never as ALTER on existing v1 tables. v1 tables are forever.

CREATE TABLE IF NOT EXISTS schema_version (
    version INTEGER PRIMARY KEY
);
INSERT OR IGNORE INTO schema_version(version) VALUES (1);

CREATE TABLE IF NOT EXISTS entries (
    seq           INTEGER PRIMARY KEY,
    prev_hash     BLOB NOT NULL,
    hash          BLOB NOT NULL UNIQUE,
    kind          INTEGER NOT NULL,
    consumer_id   BLOB NOT NULL,
    seeder_id     BLOB NOT NULL,
    model         TEXT NOT NULL DEFAULT '',
    input_tokens  INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    cost_credits  INTEGER NOT NULL,
    timestamp     INTEGER NOT NULL,
    request_id    BLOB NOT NULL,
    flags         INTEGER NOT NULL DEFAULT 0,
    ref           BLOB NOT NULL,
    consumer_sig  BLOB,
    seeder_sig    BLOB,
    tracker_sig   BLOB NOT NULL,
    canonical     BLOB NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_entries_consumer ON entries(consumer_id, seq);
CREATE INDEX IF NOT EXISTS idx_entries_seeder   ON entries(seeder_id, seq);
CREATE INDEX IF NOT EXISTS idx_entries_time     ON entries(timestamp);

CREATE TABLE IF NOT EXISTS balances (
    identity_id BLOB PRIMARY KEY,
    credits     INTEGER NOT NULL,
    last_seq    INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS merkle_roots (
    hour        INTEGER PRIMARY KEY,
    root        BLOB NOT NULL,
    tracker_sig BLOB NOT NULL
);

CREATE TABLE IF NOT EXISTS peer_root_archive (
    tracker_id  BLOB NOT NULL,
    hour        INTEGER NOT NULL,
    root        BLOB NOT NULL,
    sig         BLOB NOT NULL,
    received_at INTEGER NOT NULL,
    PRIMARY KEY (tracker_id, hour)
);

CREATE TABLE IF NOT EXISTS peer_revocations (
    tracker_id  BLOB NOT NULL,         -- issuer
    identity_id BLOB NOT NULL,         -- revoked identity
    reason      INTEGER NOT NULL,      -- RevocationReason enum value
    revoked_at  INTEGER NOT NULL,      -- unix seconds (issuer's clock)
    tracker_sig BLOB NOT NULL,         -- 64 bytes
    received_at INTEGER NOT NULL,      -- unix seconds (local clock)
    PRIMARY KEY (tracker_id, identity_id)
);

CREATE INDEX IF NOT EXISTS idx_peer_revocations_identity
    ON peer_revocations (identity_id);

CREATE TABLE IF NOT EXISTS known_peers (
    tracker_id   BLOB    NOT NULL PRIMARY KEY,
    addr         TEXT    NOT NULL,
    last_seen    INTEGER NOT NULL,           -- unix seconds
    region_hint  TEXT    NOT NULL DEFAULT '',
    health_score REAL    NOT NULL DEFAULT 0.0,
    source       TEXT    NOT NULL CHECK (source IN ('allowlist','gossip'))
);

CREATE INDEX IF NOT EXISTS idx_known_peers_health
    ON known_peers (health_score DESC);

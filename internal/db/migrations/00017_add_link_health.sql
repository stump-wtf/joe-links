-- Governing: SPEC-0020 REQ "Destination Health Checking", REQ "Lifecycle State in API and MCP", ADR-0020
-- health_checks_disabled is owner intent edited through the link form/API, so
-- it lives on links with the lifecycle timestamps; link_health is
-- machine-written, high-churn checker state, kept off the hot links table so
-- probes never touch updated_at and rollback is a clean DROP TABLE (ADR-0020
-- (c)). "Broken" is derived (consecutive_failures >= 3), never stored — the
-- same no-drift principle as the lifecycle timestamps. Plain portable SQL
-- across sqlite3/mysql/postgres (ADR-0002 — contrast migration 00015).
-- +goose Up
ALTER TABLE links ADD COLUMN health_checks_disabled BOOLEAN NOT NULL DEFAULT FALSE;

-- The FK is a table-level constraint deliberately: MySQL parses but silently
-- IGNORES inline column-level REFERENCES clauses, which would leave the table
-- with no ON DELETE CASCADE on that dialect (sqlite3/postgres honor both
-- forms). Contrast 00010/00012, which predate this lesson.
CREATE TABLE IF NOT EXISTS link_health (
    link_id TEXT NOT NULL PRIMARY KEY,
    last_checked_at TIMESTAMP NULL,
    last_status INTEGER NULL,
    last_error TEXT NULL,
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    next_check_at TIMESTAMP NULL,
    skipped BOOLEAN NOT NULL DEFAULT FALSE,
    FOREIGN KEY (link_id) REFERENCES links(id) ON DELETE CASCADE
);

-- +goose Down
DROP TABLE IF EXISTS link_health;
ALTER TABLE links DROP COLUMN health_checks_disabled;

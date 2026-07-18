-- Governing: SPEC-0020 REQ "Link Expiration", REQ "Archive State", ADR-0020
-- Lifecycle state is derived from two nullable UTC timestamps — archived when
-- archived_at is set, else expired when expires_at <= now, else active. There
-- is deliberately no status column (ADR-0020 (a)): nullable ADD COLUMN is the
-- one schema change sqlite3/mysql/postgres all perform identically with no
-- table rebuild, keeping this migration plain portable SQL (contrast 00015).
-- +goose Up
ALTER TABLE links ADD COLUMN expires_at TIMESTAMP NULL;
ALTER TABLE links ADD COLUMN archived_at TIMESTAMP NULL;

-- +goose Down
ALTER TABLE links DROP COLUMN archived_at;
ALTER TABLE links DROP COLUMN expires_at;

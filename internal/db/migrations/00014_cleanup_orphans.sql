-- Governing: SPEC-0001 REQ "Database Schema Migrations", ADR-0002
-- +goose Up
-- SQLite deployments ran with foreign_keys=OFF (the SQLite default) until
-- db.New started enabling the pragma, so ON DELETE CASCADE / SET NULL never
-- fired and parent deletes left orphaned child rows behind. Remove them so
-- the now-enforced constraints operate on consistent data. This is a no-op
-- on PostgreSQL/MySQL, where the constraints have always been enforced.
DELETE FROM link_owners WHERE link_id NOT IN (SELECT id FROM links);
DELETE FROM link_owners WHERE user_id NOT IN (SELECT id FROM users);
DELETE FROM link_tags WHERE link_id NOT IN (SELECT id FROM links);
DELETE FROM link_tags WHERE tag_id NOT IN (SELECT id FROM tags);
DELETE FROM link_shares WHERE link_id NOT IN (SELECT id FROM links);
DELETE FROM link_shares WHERE user_id NOT IN (SELECT id FROM users);
DELETE FROM link_shares WHERE shared_by NOT IN (SELECT id FROM users);
DELETE FROM link_clicks WHERE link_id NOT IN (SELECT id FROM links);
UPDATE link_clicks SET user_id = NULL
    WHERE user_id IS NOT NULL AND user_id NOT IN (SELECT id FROM users);
DELETE FROM api_tokens WHERE user_id NOT IN (SELECT id FROM users);

-- +goose Down
-- Deleted orphan rows cannot be restored; nothing to undo.
SELECT 1;

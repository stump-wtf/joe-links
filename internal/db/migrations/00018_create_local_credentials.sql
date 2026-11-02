-- Governing: SPEC-0001 REQ "Local Admin Authentication", ADR-0024
-- Local-mode accounts authenticate against a derived password hash kept here
-- instead of an identity provider. It is a separate table deliberately: OIDC
-- users MUST stay passwordless (ADR-0003), so nothing on the users row changes
-- for existing deployments, and rollback is a clean DROP TABLE. The username
-- lives on users.subject with provider = 'local' (ADR-0002 keeps one identity
-- table), which is why there is no username column here. The FK is table-level
-- because MySQL ignores inline column-level REFERENCES (contrast 00017).
-- +goose Up
CREATE TABLE IF NOT EXISTS local_credentials (
    user_id TEXT NOT NULL PRIMARY KEY,
    password_hash TEXT NOT NULL,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);

-- +goose Down
DROP TABLE IF EXISTS local_credentials;

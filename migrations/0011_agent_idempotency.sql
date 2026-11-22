-- Idempotency support for POST /agents and POST /agents/{id}/versions (docs/architecture.md section 17).
-- The key is NULL for rows created before this migration; MySQL permits many NULLs in a unique key.

ALTER TABLE agent
    ADD COLUMN config_sha256 CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER description,
    ADD COLUMN idempotency_key VARCHAR(128) NULL AFTER config_sha256,
    ADD UNIQUE KEY uq_agent_idempotency (idempotency_key);

ALTER TABLE agent_version
    ADD COLUMN idempotency_key VARCHAR(128) NULL AFTER config_sha256,
    ADD UNIQUE KEY uq_agent_version_idempotency (idempotency_key);

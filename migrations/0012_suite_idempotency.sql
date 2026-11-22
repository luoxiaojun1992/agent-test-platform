-- Idempotency support for POST /test-suites and POST /test-suites/{id}/snapshots (docs/technical/01 section 8).
-- Keys are NULL for rows created before this migration; MySQL permits many NULLs in a unique key.

ALTER TABLE test_suite
    ADD COLUMN config_sha256 CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER description,
    ADD COLUMN idempotency_key VARCHAR(128) NULL AFTER config_sha256,
    ADD UNIQUE KEY uq_test_suite_idempotency (idempotency_key);

ALTER TABLE suite_snapshot
    ADD COLUMN request_sha256 CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER content_sha256,
    ADD COLUMN idempotency_key VARCHAR(128) NULL AFTER request_sha256,
    ADD UNIQUE KEY uq_suite_snapshot_idempotency (idempotency_key);
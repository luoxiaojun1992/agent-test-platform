-- Run creation (docs/technical/06 section 2): one Target per Run, request-level idempotency, cooperative cancel.
-- test_run.idempotency_key stays unique per row and is derived from the request key; request_key keeps the original.
-- Columns are NULL for rows created before this migration.

ALTER TABLE test_run
    ADD COLUMN target_mysql_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER agent_version_id,
    ADD COLUMN request_key VARCHAR(128) NULL AFTER idempotency_key,
    ADD COLUMN request_sha256 CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER request_key,
    ADD COLUMN cancel_requested_at TIMESTAMP(6) NULL AFTER finished_at,
    ADD KEY ix_test_run_request_key (request_key),
    ADD KEY ix_test_run_agent_snapshot (agent_version_id, suite_snapshot_id),
    ADD CONSTRAINT fk_test_run_target FOREIGN KEY (target_mysql_id) REFERENCES target_mysql (id);
-- Tracks the per-Dataset-Revision execution schema in Target MySQL (see
-- docs/technical/06-sandbox-and-security.md). The execution database name is deterministic
-- from dataset_revision.id (see internal/sandbox.DeriveDatasetIdentifiers) and is created
-- exactly once, then reused unchanged by every later Test Run that references this Revision.

ALTER TABLE dataset_revision
    ADD COLUMN execution_status     VARCHAR(16) NOT NULL DEFAULT 'PENDING' AFTER content_sha256,
    ADD COLUMN execution_database   VARCHAR(128) NOT NULL AFTER execution_status,
    ADD COLUMN execution_ready_at   TIMESTAMP(6) NULL AFTER execution_database,
    ADD COLUMN execution_error_json JSON NULL AFTER execution_ready_at,
    ADD UNIQUE KEY uq_dataset_revision_execution_database (execution_database),
    ADD CONSTRAINT ck_dataset_revision_execution_status CHECK (
        execution_status IN ('PENDING', 'PROVISIONING', 'READY', 'FAILED')
    );

-- A Test Case already references its Scenario and Dataset, so Runs are created directly from Test Case Revisions.
-- The Test Suite and Suite Snapshot concepts are removed.

ALTER TABLE test_run DROP FOREIGN KEY fk_test_run_snapshot;

ALTER TABLE test_run ADD KEY ix_test_run_agent_case (agent_version_id, test_case_revision_id);

ALTER TABLE test_run
    DROP KEY uq_test_run_identity,
    DROP KEY ix_test_run_agent_snapshot;

ALTER TABLE test_run DROP COLUMN suite_snapshot_id;

DROP TABLE suite_snapshot_case;
DROP TABLE suite_snapshot;
DROP TABLE test_suite;

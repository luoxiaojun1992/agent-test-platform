-- Splits a Generation Job into one of three single-output kinds (SCENARIO, DATASET,
-- TEST_CASES), each with its own precondition on already-published upstream Revisions, adds
-- the Generation Pipeline orchestration record that chains the three kinds together for the
-- "one description in, full package out" convenience flow, and adds an optional manual review
-- gate before publishing. See docs/architecture.md sections 6 and 9, and
-- docs/technical/01-storage-and-workflows.md sections 3, 5, 6.

CREATE TABLE generation_pipeline (
    id                          CHAR(26) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    status                      VARCHAR(16) NOT NULL DEFAULT 'RUNNING',
    requested_test_case_count  SMALLINT UNSIGNED NOT NULL,
    scenario_job_id             CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    dataset_job_id              CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    test_cases_job_id           CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    error_json                  JSON NULL,
    created_at                  TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at                  TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    CONSTRAINT fk_generation_pipeline_scenario_job FOREIGN KEY (scenario_job_id) REFERENCES generation_job(id),
    CONSTRAINT fk_generation_pipeline_dataset_job FOREIGN KEY (dataset_job_id) REFERENCES generation_job(id),
    CONSTRAINT fk_generation_pipeline_test_cases_job FOREIGN KEY (test_cases_job_id) REFERENCES generation_job(id),
    CONSTRAINT ck_generation_pipeline_status CHECK (status IN ('RUNNING', 'COMPLETED', 'FAILED', 'CANCELLED'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

ALTER TABLE generation_job
    ADD COLUMN job_kind                    VARCHAR(16) NOT NULL DEFAULT 'SCENARIO' AFTER retry_of_job_id,
    ADD COLUMN pipeline_id                 CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER job_kind,
    ADD COLUMN input_scenario_revision_id  CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER pipeline_id,
    ADD COLUMN input_dataset_revision_id   CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER input_scenario_revision_id,
    MODIFY COLUMN requested_test_case_count SMALLINT UNSIGNED NOT NULL DEFAULT 0,
    ADD COLUMN require_manual_review       BOOLEAN NOT NULL DEFAULT FALSE AFTER repair_round,
    ADD COLUMN review_status               VARCHAR(16) NULL AFTER require_manual_review,
    ADD COLUMN review_note                 TEXT NULL AFTER review_status,
    ADD COLUMN reviewer                    VARCHAR(255) NULL AFTER review_note,
    ADD COLUMN reviewed_at                 TIMESTAMP(6) NULL AFTER reviewer,
    ADD KEY ix_generation_job_pipeline (pipeline_id),
    ADD KEY ix_generation_job_input_scenario (input_scenario_revision_id),
    ADD KEY ix_generation_job_input_dataset (input_dataset_revision_id),
    ADD CONSTRAINT fk_generation_job_pipeline FOREIGN KEY (pipeline_id) REFERENCES generation_pipeline(id),
    ADD CONSTRAINT fk_generation_job_input_scenario FOREIGN KEY (input_scenario_revision_id) REFERENCES scenario_revision(id),
    ADD CONSTRAINT fk_generation_job_input_dataset FOREIGN KEY (input_dataset_revision_id) REFERENCES dataset_revision(id),
    ADD CONSTRAINT ck_generation_job_kind CHECK (job_kind IN ('SCENARIO', 'DATASET', 'TEST_CASES')),
    -- SCENARIO jobs never carry an input reference (they have nothing upstream to reference).
    -- DATASET/TEST_CASES jobs are expected to carry their required input references once job
    -- creation becomes job_kind-aware (docs/architecture.md 6.2); that expectation is enforced
    -- at the application layer (generation.JobState.Validate) rather than here, so existing
    -- callers that do not yet set job_kind-specific fields are not hard-blocked mid-migration.
    ADD CONSTRAINT ck_generation_job_scenario_kind_no_inputs CHECK (
        job_kind != 'SCENARIO' OR (input_scenario_revision_id IS NULL AND input_dataset_revision_id IS NULL)
    ),
    ADD CONSTRAINT ck_generation_job_review_status CHECK (
        review_status IS NULL OR review_status IN ('PENDING', 'APPROVED', 'REJECTED')
    );

-- A Dataset always has a Scenario context; a Test Case always has a Scenario and a Dataset
-- context. This makes the precondition in 6.2 a hard schema-level fact, not just a job-time
-- convention, and lets multiple Datasets or Test Case batches share one upstream Scenario.
ALTER TABLE dataset
    ADD COLUMN scenario_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL AFTER id,
    ADD KEY ix_dataset_scenario (scenario_id),
    ADD CONSTRAINT fk_dataset_scenario FOREIGN KEY (scenario_id) REFERENCES scenario(id);

ALTER TABLE dataset_revision
    ADD COLUMN scenario_revision_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL AFTER dataset_id,
    ADD KEY ix_dataset_revision_scenario (scenario_revision_id),
    ADD CONSTRAINT fk_dataset_revision_scenario FOREIGN KEY (scenario_revision_id) REFERENCES scenario_revision(id);

ALTER TABLE test_case
    ADD COLUMN scenario_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL AFTER id,
    ADD COLUMN dataset_id  CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL AFTER scenario_id,
    ADD KEY ix_test_case_scenario (scenario_id),
    ADD KEY ix_test_case_dataset (dataset_id),
    ADD CONSTRAINT fk_test_case_scenario FOREIGN KEY (scenario_id) REFERENCES scenario(id),
    ADD CONSTRAINT fk_test_case_dataset FOREIGN KEY (dataset_id) REFERENCES dataset(id);

-- Generation Package now bundles one Pipeline's three chained Jobs instead of one Job's three
-- outputs, since a single Job only ever produces one kind of output (see 6.2/6.3).
ALTER TABLE generation_package
    DROP FOREIGN KEY fk_generation_package_job,
    DROP KEY uq_generation_package_job,
    DROP COLUMN generation_job_id,
    ADD COLUMN pipeline_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL AFTER id,
    ADD UNIQUE KEY uq_generation_package_pipeline (pipeline_id),
    ADD CONSTRAINT fk_generation_package_pipeline FOREIGN KEY (pipeline_id) REFERENCES generation_pipeline(id);

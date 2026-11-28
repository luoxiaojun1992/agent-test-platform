-- Multi-turn test cases and a composable run pipeline (docs/technical/10).
-- Test Case content becomes a turns array; the Run stores its derived pipeline DAG (with config,
-- context, and result) and per-iteration/per-turn progress.

-- Back-fill single-turn Test Case content (schema_version 1.0) into a one-element turns array.
UPDATE test_case_revision
SET content_json = JSON_OBJECT(
        'schema_version', '2.0',
        'scenario_revision_id', JSON_UNQUOTE(JSON_EXTRACT(content_json, '$.scenario_revision_id')),
        'dataset_revision_id', JSON_UNQUOTE(JSON_EXTRACT(content_json, '$.dataset_revision_id')),
        'turns', JSON_ARRAY(JSON_OBJECT(
            'turn', 1,
            'prompt', JSON_UNQUOTE(JSON_EXTRACT(content_json, '$.prompt')),
            'expected_output', JSON_EXTRACT(content_json, '$.expected_output'),
            'evaluation_criteria', JSON_EXTRACT(content_json, '$.evaluation_criteria')
        )),
        'knowledge_documents', JSON_EXTRACT(content_json, '$.knowledge_documents'),
        'difficulty', JSON_UNQUOTE(JSON_EXTRACT(content_json, '$.difficulty')),
        'difficulty_profile', JSON_EXTRACT(content_json, '$.difficulty_profile'),
        'generation_assumptions', JSON_EXTRACT(content_json, '$.generation_assumptions')
    )
WHERE JSON_EXTRACT(content_json, '$.turns') IS NULL
  AND JSON_EXTRACT(content_json, '$.prompt') IS NOT NULL;

-- The Run stores its pipeline document (DAG + config + context + result), the turn total, and the
-- final aggregated verdict (mean of every completed iteration's last judge). Progress (current
-- iteration, teardown handles, per-node results) lives inside pipeline_json's context and result,
-- so no separate progress columns are needed.
ALTER TABLE test_run
    ADD COLUMN pipeline_json JSON NULL AFTER session_id,
    ADD COLUMN turns_total SMALLINT UNSIGNED NULL AFTER pipeline_json,
    ADD COLUMN final_score DECIMAL(5,2) NULL AFTER turns_total,
    ADD COLUMN pass BOOLEAN NULL AFTER final_score;

-- One row per turn per iteration: its prompt, execution status, judge verdict, and context
-- watermark. The iteration column lets the same turn run again in a later repetition.
CREATE TABLE test_run_turn (
    id                CHAR(26) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    test_run_id       CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    iteration         SMALLINT UNSIGNED NOT NULL,
    turn_no           SMALLINT UNSIGNED NOT NULL,
    prompt            MEDIUMTEXT NOT NULL,
    status            VARCHAR(32) NOT NULL,
    final_output      MEDIUMTEXT NULL,
    output_sha256     CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL,
    context_watermark INT UNSIGNED NULL,
    llm_score         DECIMAL(5,2) NULL,
    final_score       DECIMAL(5,2) NULL,
    pass              BOOLEAN NULL,
    error_json        JSON NULL,
    started_at        TIMESTAMP(6) NULL,
    finished_at       TIMESTAMP(6) NULL,
    created_at        TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    UNIQUE KEY uq_test_run_turn (test_run_id, iteration, turn_no),
    CONSTRAINT fk_test_run_turn_run FOREIGN KEY (test_run_id) REFERENCES test_run(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- Each judge attempt is attributable to an iteration and a turn.
ALTER TABLE evaluation_attempt ADD COLUMN iteration SMALLINT UNSIGNED NULL;
ALTER TABLE evaluation_attempt ADD COLUMN turn_no SMALLINT UNSIGNED NULL;

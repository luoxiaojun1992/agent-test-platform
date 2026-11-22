-- Platform MySQL schema. Apply only to the dedicated Platform MySQL instance.
-- MySQL 8.4 LTS, UTC, utf8mb4, strict SQL mode.

CREATE TABLE database_profile (
    id                  CHAR(26) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    name                VARCHAR(255) NOT NULL,
    engine              VARCHAR(32) NOT NULL,
    host                VARCHAR(255) NOT NULL,
    port                SMALLINT UNSIGNED NOT NULL,
    database_name       VARCHAR(128) NOT NULL,
    username            VARCHAR(128) NOT NULL,
    password_secret_ref VARCHAR(512) NOT NULL,
    tls_mode            VARCHAR(32) NOT NULL,
    snapshot_mode       VARCHAR(32) NOT NULL,
    snapshot_timeout_seconds SMALLINT UNSIGNED NOT NULL DEFAULT 300,
    config_json         JSON NOT NULL,
    config_sha256       CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    created_at          TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at          TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    UNIQUE KEY uq_database_profile_name (name),
    UNIQUE KEY uq_database_profile_digest (config_sha256),
    CONSTRAINT ck_database_profile_engine CHECK (engine = 'MYSQL')
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE agent (
    id                      CHAR(26) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    name                    VARCHAR(255) NOT NULL,
    description             TEXT NULL,
    database_profile_id     CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    chat_endpoint           VARCHAR(2048) NOT NULL,
    target_bearer_token_ref VARCHAR(512) NOT NULL,
    target_model            VARCHAR(255) NOT NULL,
    contract_version        VARCHAR(32) NOT NULL,
    tool_evaluation_mode    VARCHAR(32) NOT NULL,
    tool_catalog_json       JSON NULL,
    config_json             JSON NOT NULL,
    config_sha256           CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    idempotency_key         VARCHAR(128) NULL,
    created_at              TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at              TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    UNIQUE KEY uq_agent_name (name),
    UNIQUE KEY uq_agent_idempotency (idempotency_key),
    CONSTRAINT fk_agent_database_profile FOREIGN KEY (database_profile_id) REFERENCES database_profile(id),
    CONSTRAINT ck_agent_tool_mode CHECK (
        (tool_evaluation_mode = 'DETERMINISTIC_CHAIN' AND tool_catalog_json IS NOT NULL)
        OR (tool_evaluation_mode = 'SESSION_LLM_ONLY' AND tool_catalog_json IS NULL)
    )
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE model_config (
    id                  CHAR(26) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    alias_name          VARCHAR(128) NOT NULL,
    revision_no         INT UNSIGNED NOT NULL,
    model_role          VARCHAR(32) NOT NULL,
    provider            VARCHAR(32) NOT NULL,
    model_name          VARCHAR(255) NOT NULL,
    base_url            VARCHAR(2048) NOT NULL,
    credential_ref      VARCHAR(512) NULL,
    capabilities_json   JSON NOT NULL,
    config_json         JSON NOT NULL,
    config_sha256       CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    enabled             BOOLEAN NOT NULL DEFAULT FALSE,
    created_at          TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    UNIQUE KEY uq_model_config_revision (alias_name, revision_no),
    UNIQUE KEY uq_model_config_digest (config_sha256),
    KEY ix_model_config_role (model_role, enabled)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE model_capability_probe (
    id                  CHAR(26) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    model_config_id     CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    capability_name     VARCHAR(64) NOT NULL,
    status              VARCHAR(16) NOT NULL,
    latency_ms          BIGINT UNSIGNED NULL,
    details_json        JSON NULL,
    probed_at           TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    UNIQUE KEY uq_model_capability_probe (model_config_id, capability_name),
    CONSTRAINT fk_model_probe_config FOREIGN KEY (model_config_id) REFERENCES model_config(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE judge_calibration (
    id                  CHAR(26) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    model_config_id     CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    calibration_version VARCHAR(128) NOT NULL,
    status              VARCHAR(16) NOT NULL,
    sample_count        INT UNSIGNED NOT NULL,
    hard_failure_count  INT UNSIGNED NOT NULL,
    agreement_rate      DECIMAL(5,4) NOT NULL,
    hard_false_passes   INT UNSIGNED NOT NULL,
    report_json         JSON NOT NULL,
    calibrated_at       TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    UNIQUE KEY uq_judge_calibration (model_config_id, calibration_version),
    CONSTRAINT fk_judge_calibration_config FOREIGN KEY (model_config_id) REFERENCES model_config(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE generation_request (
    id                  CHAR(26) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    idempotency_key     VARCHAR(128) NOT NULL,
    request_json        JSON NOT NULL,
    request_sha256      CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    description_tokens  INT UNSIGNED NOT NULL,
    target_agent_id     CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    generate_dataset    BOOLEAN NOT NULL DEFAULT TRUE,
    database_profile_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    created_at          TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    UNIQUE KEY uq_generation_request_idempotency (idempotency_key),
    CONSTRAINT fk_generation_request_agent FOREIGN KEY (target_agent_id) REFERENCES agent(id),
    CONSTRAINT fk_generation_request_database_profile FOREIGN KEY (database_profile_id) REFERENCES database_profile(id),
    CONSTRAINT ck_generation_request_dataset_source CHECK (
        (generate_dataset = TRUE AND database_profile_id IS NULL)
        OR (generate_dataset = FALSE AND database_profile_id IS NOT NULL)
    )
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE generation_job (
    id                          CHAR(26) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    request_id                  CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    retry_of_job_id             CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    status                      VARCHAR(32) NOT NULL,
    current_stage               VARCHAR(64) NULL,
    requested_test_case_count   SMALLINT UNSIGNED NOT NULL,
    accepted_test_case_count    SMALLINT UNSIGNED NOT NULL DEFAULT 0,
    repair_round                SMALLINT UNSIGNED NOT NULL DEFAULT 0,
    generator_config_sha256     CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    error_json                  JSON NULL,
    published_package_id        CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    started_at                  TIMESTAMP(6) NULL,
    finished_at                 TIMESTAMP(6) NULL,
    created_at                  TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at                  TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    UNIQUE KEY uq_generation_job_request (request_id),
    KEY ix_generation_job_status (status, created_at),
    CONSTRAINT fk_generation_job_request FOREIGN KEY (request_id) REFERENCES generation_request(id),
    CONSTRAINT fk_generation_job_retry FOREIGN KEY (retry_of_job_id) REFERENCES generation_job(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE work_item (
    id                  CHAR(26) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    queue_name          VARCHAR(64) NOT NULL,
    aggregate_type      VARCHAR(64) NOT NULL,
    aggregate_id        CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    stage_key           VARCHAR(128) NOT NULL,
    status              VARCHAR(16) NOT NULL,
    priority            INT NOT NULL DEFAULT 0,
    available_at        TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    lease_owner         VARCHAR(128) NULL,
    lease_until         TIMESTAMP(6) NULL,
    fencing_token       BIGINT UNSIGNED NOT NULL DEFAULT 0,
    attempt_count       INT UNSIGNED NOT NULL DEFAULT 0,
    max_attempts        INT UNSIGNED NOT NULL DEFAULT 3,
    payload_json        JSON NOT NULL,
    last_error_json     JSON NULL,
    created_at          TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at          TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    UNIQUE KEY uq_work_item_stage (queue_name, aggregate_type, aggregate_id, stage_key),
    KEY ix_work_item_claim (queue_name, status, available_at, priority, created_at),
    KEY ix_work_item_lease (status, lease_until)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE generation_artifact_draft (
    id                  CHAR(26) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    generation_job_id   CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    artifact_kind       VARCHAR(32) NOT NULL,
    artifact_key        VARCHAR(128) NOT NULL,
    content_json        JSON NULL,
    content_text        MEDIUMTEXT NULL,
    content_sha256      CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    validation_json     JSON NULL,
    expires_at          TIMESTAMP(6) NOT NULL,
    created_at          TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    UNIQUE KEY uq_generation_artifact_draft (generation_job_id, artifact_kind, artifact_key),
    KEY ix_generation_artifact_expiry (expires_at),
    CONSTRAINT fk_generation_artifact_job FOREIGN KEY (generation_job_id) REFERENCES generation_job(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE generation_test_case_draft (
    id                  CHAR(26) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    generation_job_id   CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    client_case_id      VARCHAR(64) NOT NULL,
    schema_version      VARCHAR(32) NOT NULL,
    content_json        JSON NOT NULL,
    content_sha256      CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    accepted            BOOLEAN NOT NULL DEFAULT FALSE,
    validation_json     JSON NOT NULL,
    expires_at          TIMESTAMP(6) NOT NULL,
    created_at          TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at          TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    UNIQUE KEY uq_generation_test_case_draft (generation_job_id, client_case_id),
    KEY ix_generation_test_case_expiry (expires_at),
    CONSTRAINT fk_generation_test_case_job FOREIGN KEY (generation_job_id) REFERENCES generation_job(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE scenario (
    id                  CHAR(26) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    created_at          TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE scenario_revision (
    id                  CHAR(26) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    scenario_id         CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    generation_job_id   CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    revision_no         INT UNSIGNED NOT NULL,
    quality_level       VARCHAR(32) NOT NULL,
    content_json        JSON NOT NULL,
    content_sha256      CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    created_at          TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    UNIQUE KEY uq_scenario_revision (scenario_id, revision_no),
    UNIQUE KEY uq_scenario_revision_digest (scenario_id, content_sha256),
    CONSTRAINT fk_scenario_revision_scenario FOREIGN KEY (scenario_id) REFERENCES scenario(id),
    CONSTRAINT fk_scenario_revision_job FOREIGN KEY (generation_job_id) REFERENCES generation_job(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE dataset (
    id                  CHAR(26) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    created_at          TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE dataset_revision (
    id                  CHAR(26) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    dataset_id          CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    generation_job_id   CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    revision_no         INT UNSIGNED NOT NULL,
    schema_sql          MEDIUMTEXT NOT NULL,
    seed_sql            MEDIUMTEXT NOT NULL,
    constraints_json    JSON NOT NULL,
    metadata_json       JSON NOT NULL,
    content_sha256      CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    created_at          TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    UNIQUE KEY uq_dataset_revision (dataset_id, revision_no),
    UNIQUE KEY uq_dataset_revision_digest (dataset_id, content_sha256),
    CONSTRAINT fk_dataset_revision_dataset FOREIGN KEY (dataset_id) REFERENCES dataset(id),
    CONSTRAINT fk_dataset_revision_job FOREIGN KEY (generation_job_id) REFERENCES generation_job(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE test_case (
    id                  CHAR(26) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    created_at          TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE test_case_revision (
    id                      CHAR(26) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    test_case_id            CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    generation_job_id       CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    scenario_revision_id    CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    dataset_revision_id     CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    revision_no             INT UNSIGNED NOT NULL,
    schema_version          VARCHAR(32) NOT NULL,
    agent_mode              VARCHAR(16) NOT NULL,
    tool_evaluation_mode    VARCHAR(32) NOT NULL,
    content_json            JSON NOT NULL,
    content_sha256          CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    lifecycle_status        VARCHAR(32) NOT NULL,
    created_at              TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    UNIQUE KEY uq_test_case_revision (test_case_id, revision_no),
    UNIQUE KEY uq_test_case_revision_digest (test_case_id, content_sha256),
    KEY ix_test_case_revision_scenario (scenario_revision_id),
    KEY ix_test_case_revision_dataset (dataset_revision_id),
    CONSTRAINT fk_test_case_revision_case FOREIGN KEY (test_case_id) REFERENCES test_case(id),
    CONSTRAINT fk_test_case_revision_job FOREIGN KEY (generation_job_id) REFERENCES generation_job(id),
    CONSTRAINT fk_test_case_revision_scenario FOREIGN KEY (scenario_revision_id) REFERENCES scenario_revision(id),
    CONSTRAINT fk_test_case_revision_dataset FOREIGN KEY (dataset_revision_id) REFERENCES dataset_revision(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE generation_package (
    id                      CHAR(26) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    generation_job_id       CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    scenario_revision_id    CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    dataset_revision_id     CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    manifest_json           JSON NOT NULL,
    manifest_sha256         CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    created_at              TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    UNIQUE KEY uq_generation_package_job (generation_job_id),
    CONSTRAINT fk_generation_package_job FOREIGN KEY (generation_job_id) REFERENCES generation_job(id),
    CONSTRAINT fk_generation_package_scenario FOREIGN KEY (scenario_revision_id) REFERENCES scenario_revision(id),
    CONSTRAINT fk_generation_package_dataset FOREIGN KEY (dataset_revision_id) REFERENCES dataset_revision(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE generation_package_test_case (
    generation_package_id   CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    test_case_revision_id   CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    ordinal                 SMALLINT UNSIGNED NOT NULL,
    PRIMARY KEY (generation_package_id, test_case_revision_id),
    UNIQUE KEY uq_generation_package_ordinal (generation_package_id, ordinal),
    CONSTRAINT fk_package_case_package FOREIGN KEY (generation_package_id) REFERENCES generation_package(id),
    CONSTRAINT fk_package_case_revision FOREIGN KEY (test_case_revision_id) REFERENCES test_case_revision(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE test_suite (
    id                  CHAR(26) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    name                VARCHAR(255) NOT NULL,
    description         TEXT NULL,
    created_at          TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at          TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    UNIQUE KEY uq_test_suite_name (name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE suite_snapshot (
    id                  CHAR(26) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    test_suite_id       CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    revision_no         INT UNSIGNED NOT NULL,
    content_sha256      CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    created_at          TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    UNIQUE KEY uq_suite_snapshot_revision (test_suite_id, revision_no),
    CONSTRAINT fk_suite_snapshot_suite FOREIGN KEY (test_suite_id) REFERENCES test_suite(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE suite_snapshot_case (
    suite_snapshot_id       CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    test_case_revision_id   CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    ordinal                 INT UNSIGNED NOT NULL,
    PRIMARY KEY (suite_snapshot_id, test_case_revision_id),
    UNIQUE KEY uq_suite_snapshot_ordinal (suite_snapshot_id, ordinal),
    CONSTRAINT fk_snapshot_case_snapshot FOREIGN KEY (suite_snapshot_id) REFERENCES suite_snapshot(id),
    CONSTRAINT fk_snapshot_case_revision FOREIGN KEY (test_case_revision_id) REFERENCES test_case_revision(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE test_run (
    id                      CHAR(26) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    idempotency_key         VARCHAR(128) NOT NULL,
    suite_snapshot_id       CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    test_case_revision_id   CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    dataset_revision_id     CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    agent_id                CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    repetition_no           SMALLINT UNSIGNED NOT NULL DEFAULT 1,
    status                  VARCHAR(32) NOT NULL,
    execution_outcome       VARCHAR(32) NULL,
    input_text              MEDIUMTEXT NOT NULL,
    final_output            MEDIUMTEXT NULL,
    precondition_json       JSON NULL,
    postcondition_json      JSON NULL,
    state_diff_json         JSON NULL,
    deterministic_json      JSON NULL,
    deterministic_score     DECIMAL(5,2) NULL,
    actual_output_sha256    CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL,
    error_json              JSON NULL,
    started_at              TIMESTAMP(6) NULL,
    agent_finished_at       TIMESTAMP(6) NULL,
    environment_cleaned_at  TIMESTAMP(6) NULL,
    finished_at             TIMESTAMP(6) NULL,
    created_at              TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at              TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    UNIQUE KEY uq_test_run_idempotency (idempotency_key),
    UNIQUE KEY uq_test_run_identity (suite_snapshot_id, agent_id, test_case_revision_id, repetition_no),
    KEY ix_test_run_status (status, created_at),
    CONSTRAINT fk_test_run_snapshot FOREIGN KEY (suite_snapshot_id) REFERENCES suite_snapshot(id),
    CONSTRAINT fk_test_run_case FOREIGN KEY (test_case_revision_id) REFERENCES test_case_revision(id),
    CONSTRAINT fk_test_run_dataset FOREIGN KEY (dataset_revision_id) REFERENCES dataset_revision(id),
    CONSTRAINT fk_test_run_agent FOREIGN KEY (agent_id) REFERENCES agent(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE target_knowledge_document (
    id                  CHAR(26) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    test_run_id         CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    ordinal             SMALLINT UNSIGNED NOT NULL,
    name                VARCHAR(255) NOT NULL,
    source_sha256       CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    target_document_id  VARCHAR(128) NULL,
    status              VARCHAR(16) NOT NULL,
    indexing_deadline   TIMESTAMP(6) NOT NULL,
    expires_at          TIMESTAMP(6) NOT NULL,
    indexed_at          TIMESTAMP(6) NULL,
    error_json          JSON NULL,
    created_at          TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at          TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    UNIQUE KEY uq_target_knowledge_ordinal (test_run_id, ordinal),
    UNIQUE KEY uq_target_knowledge_name (test_run_id, name),
    UNIQUE KEY uq_target_knowledge_document (test_run_id, target_document_id),
    KEY ix_target_knowledge_status (status, indexing_deadline),
    CONSTRAINT fk_target_knowledge_run FOREIGN KEY (test_run_id) REFERENCES test_run(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE run_tool_grant (
    id                  CHAR(26) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    test_run_id         CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    token_sha256        CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    allowed_tools_json  JSON NOT NULL,
    expires_at          TIMESTAMP(6) NOT NULL,
    revoked_at          TIMESTAMP(6) NULL,
    created_at          TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    UNIQUE KEY uq_run_tool_grant_token (token_sha256),
    KEY ix_run_tool_grant_run (test_run_id),
    KEY ix_run_tool_grant_expiry (expires_at, revoked_at),
    CONSTRAINT fk_run_tool_grant_run FOREIGN KEY (test_run_id) REFERENCES test_run(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE tool_gateway_call (
    id                  CHAR(26) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    test_run_id         CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    call_id             VARCHAR(128) NOT NULL,
    arrival_sequence    SMALLINT UNSIGNED NOT NULL,
    tool_name           VARCHAR(128) NOT NULL,
    arguments_json      JSON NOT NULL,
    arguments_sha256    CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    result_json         JSON NULL,
    result_sha256       CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL,
    error_json          JSON NULL,
    error_sha256        CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL,
    status              VARCHAR(16) NOT NULL,
    truncated           BOOLEAN NOT NULL DEFAULT FALSE,
    original_bytes      INT UNSIGNED NULL,
    duration_ms         BIGINT UNSIGNED NOT NULL,
    created_at          TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    UNIQUE KEY uq_tool_gateway_call_id (test_run_id, call_id),
    UNIQUE KEY uq_tool_gateway_sequence (test_run_id, arrival_sequence),
    CONSTRAINT fk_tool_gateway_call_run FOREIGN KEY (test_run_id) REFERENCES test_run(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE tool_call_record (
    id                  CHAR(26) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    test_run_id         CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    sequence_no         SMALLINT UNSIGNED NOT NULL,
    call_id             VARCHAR(128) NOT NULL,
    parent_call_id      VARCHAR(128) NULL,
    tool_name           VARCHAR(128) NOT NULL,
    arguments_json      JSON NOT NULL,
    arguments_sha256    CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    result_json         JSON NULL,
    result_sha256       CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL,
    error_json          JSON NULL,
    error_sha256        CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL,
    status              VARCHAR(16) NOT NULL,
    trace_source        VARCHAR(32) NOT NULL,
    truncated           BOOLEAN NOT NULL DEFAULT FALSE,
    original_bytes      INT UNSIGNED NULL,
    duration_ms         BIGINT UNSIGNED NOT NULL,
    started_at          TIMESTAMP(6) NULL,
    created_at          TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    UNIQUE KEY uq_tool_call_sequence (test_run_id, sequence_no),
    UNIQUE KEY uq_tool_call_id (test_run_id, call_id),
    KEY ix_tool_call_name (tool_name),
    CONSTRAINT fk_tool_call_run FOREIGN KEY (test_run_id) REFERENCES test_run(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE agent_session_event (
    id                  CHAR(26) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    test_run_id         CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    sequence_no         SMALLINT UNSIGNED NOT NULL,
    event_id            VARCHAR(128) NOT NULL,
    event_type          VARCHAR(64) NOT NULL,
    event_json          JSON NOT NULL,
    event_sha256        CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    created_at          TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    UNIQUE KEY uq_agent_session_event_sequence (test_run_id, sequence_no),
    UNIQUE KEY uq_agent_session_event_id (test_run_id, event_id),
    KEY ix_agent_session_event_type (test_run_id, event_type),
    CONSTRAINT fk_agent_session_event_run FOREIGN KEY (test_run_id) REFERENCES test_run(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE evaluation_attempt (
    id                      CHAR(26) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    test_run_id             CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    attempt_no              SMALLINT UNSIGNED NOT NULL,
    status                  VARCHAR(16) NOT NULL,
    judge_model_config_id   CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    judge_model_alias       VARCHAR(128) NOT NULL,
    judge_provider          VARCHAR(32) NOT NULL,
    judge_model             VARCHAR(255) NOT NULL,
    judge_prompt_version    VARCHAR(128) NOT NULL,
    judge_config_sha256     CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    request_json            JSON NOT NULL,
    response_json           JSON NULL,
    criteria_json           JSON NULL,
    failure_categories_json JSON NULL,
    llm_score               DECIMAL(5,2) NULL,
    final_score             DECIMAL(5,2) NULL,
    pass                    BOOLEAN NULL,
    error_json              JSON NULL,
    provider_request_id     VARCHAR(255) NULL,
    input_tokens            INT UNSIGNED NULL,
    output_tokens           INT UNSIGNED NULL,
    started_at              TIMESTAMP(6) NULL,
    finished_at             TIMESTAMP(6) NULL,
    created_at              TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    UNIQUE KEY uq_evaluation_attempt (test_run_id, attempt_no),
    UNIQUE KEY uq_evaluation_config (test_run_id, judge_config_sha256, attempt_no),
    KEY ix_evaluation_status (status, created_at),
    CONSTRAINT fk_evaluation_run FOREIGN KEY (test_run_id) REFERENCES test_run(id),
    CONSTRAINT fk_evaluation_model FOREIGN KEY (judge_model_config_id) REFERENCES model_config(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

ALTER TABLE generation_job
    ADD CONSTRAINT fk_generation_job_package
    FOREIGN KEY (published_package_id) REFERENCES generation_package(id);

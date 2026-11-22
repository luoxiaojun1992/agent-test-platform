-- The platform has no Tool Gateway: the Target Agent owns its Tools and the platform only judges the reported trace.
DROP TABLE tool_gateway_call;
DROP TABLE run_tool_grant;

-- Evaluation Attempts exist from the moment they are requested (PENDING), before a Judge configuration is resolved,
-- so the Judge snapshot and request columns are filled only when the worker starts the attempt.
ALTER TABLE evaluation_attempt
    MODIFY COLUMN judge_model_config_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    MODIFY COLUMN judge_provider VARCHAR(32) NULL,
    MODIFY COLUMN judge_model VARCHAR(255) NULL,
    MODIFY COLUMN judge_prompt_version VARCHAR(128) NULL,
    MODIFY COLUMN judge_config_sha256 CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL,
    MODIFY COLUMN request_json JSON NULL;
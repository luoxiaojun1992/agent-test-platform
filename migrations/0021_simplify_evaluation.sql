-- Simplify evaluation: collapse the evaluation modes and remove deterministic state verification
-- (docs/technical/09). The Agent tool catalog stays; it now informs Test Case generation.

-- The Agent no longer distinguishes DETERMINISTIC_CHAIN vs SESSION_LLM_ONLY.
ALTER TABLE agent DROP CHECK ck_agent_tool_mode;
ALTER TABLE agent DROP COLUMN tool_evaluation_mode;

-- Test Runs no longer store deterministic state verification; they record the Target session id.
ALTER TABLE test_run
    DROP COLUMN precondition_json,
    DROP COLUMN postcondition_json,
    DROP COLUMN state_diff_json,
    DROP COLUMN deterministic_json,
    DROP COLUMN deterministic_score,
    ADD COLUMN session_id VARCHAR(128) NULL AFTER agent_id;

-- Test Case Revisions no longer carry the mode discriminator columns.
ALTER TABLE test_case_revision
    DROP COLUMN agent_mode,
    DROP COLUMN tool_evaluation_mode;

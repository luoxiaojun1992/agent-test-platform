-- At most one enabled (active) model per role: the generation model and the judge model are each selected by alias.
UPDATE model_config SET enabled = FALSE;
ALTER TABLE model_config
    ADD COLUMN active_role VARCHAR(32) GENERATED ALWAYS AS (IF(enabled, model_role, NULL)) STORED,
    ADD UNIQUE KEY uq_model_config_active_role (active_role);

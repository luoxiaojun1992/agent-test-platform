-- Gives Scenario and Dataset Revisions the same Asset Revision lifecycle that Test Case
-- Revisions already have (docs/architecture.md 15.2: STAGING -> VALIDATED -> ACTIVE ->
-- DEPRECATED), so DATASET and TEST_CASES Jobs can require ACTIVE/VALIDATED inputs (6.2) and
-- Atomic Publish step 6 can apply auto_activate to every new Revision (technical 01 section 6).
-- Also stores each Job's own provenance (model, prompt, parameter, and critic records) on the
-- Job at publish time, so a Generation Manifest can be assembled from durable rows after the
-- staging drafts it was produced from have been purged (technical 01 section 6, step 7).

ALTER TABLE scenario_revision
    ADD COLUMN lifecycle_status VARCHAR(32) NOT NULL AFTER quality_level,
    ADD CONSTRAINT ck_scenario_revision_lifecycle CHECK (lifecycle_status IN ('STAGING', 'VALIDATED', 'ACTIVE', 'DEPRECATED'));

ALTER TABLE dataset_revision
    ADD COLUMN lifecycle_status VARCHAR(32) NOT NULL AFTER content_sha256,
    ADD CONSTRAINT ck_dataset_revision_lifecycle CHECK (lifecycle_status IN ('STAGING', 'VALIDATED', 'ACTIVE', 'DEPRECATED'));

ALTER TABLE generation_job
    ADD COLUMN provenance_json JSON NULL AFTER published_package_id;

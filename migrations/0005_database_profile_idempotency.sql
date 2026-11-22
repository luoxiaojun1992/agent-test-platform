-- Stores the Idempotency-Key a Database Profile was created with, following the same pattern as
-- generation_request.idempotency_key. docs/technical/05-api-and-implementation-plan.md section 2
-- requires Idempotency-Key on every create operation; a replayed create with the same key and the
-- same configuration returns the existing profile, and the same key with a different configuration
-- is an idempotency conflict.

ALTER TABLE database_profile
    ADD COLUMN idempotency_key VARCHAR(128) NOT NULL AFTER config_sha256,
    ADD UNIQUE KEY uq_database_profile_idempotency (idempotency_key);

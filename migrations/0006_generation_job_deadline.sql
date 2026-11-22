-- Gives each Generation Job its 10-minute processing budget (docs/architecture.md, limits table:
-- "Generation job duration"). deadline_at is fixed when the Job first advances past acceptance, so
-- every Worker attempt, including one that takes over an expired Lease, sees the same deadline. The
-- budget is wall-clock time, so time spent waiting in AWAITING_REVIEW counts toward it.

ALTER TABLE generation_job
    ADD COLUMN deadline_at TIMESTAMP(6) NULL AFTER finished_at;

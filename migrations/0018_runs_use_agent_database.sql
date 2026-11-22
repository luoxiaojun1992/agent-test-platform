-- Seed data is always written into the Agent's own database, so a Run no longer names a Target MySQL.

ALTER TABLE test_run DROP FOREIGN KEY fk_test_run_target;
ALTER TABLE test_run DROP COLUMN target_mysql_id;

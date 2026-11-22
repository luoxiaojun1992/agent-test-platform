-- Target MySQL is the platform's own validation server, not part of any Agent. Rename its objects accordingly.

RENAME TABLE target_mysql TO validation_mysql, dataset_revision_target TO dataset_revision_validation_db;

ALTER TABLE dataset_revision_validation_db RENAME COLUMN target_mysql_id TO validation_db_id;

-- The database that receives seed data belongs to one Agent (its account is unique to that Agent), not to a version.

ALTER TABLE agent
    ADD COLUMN database_profile_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER description,
    ADD CONSTRAINT fk_agent_database_profile FOREIGN KEY (database_profile_id) REFERENCES database_profile(id);

UPDATE agent a JOIN (
    SELECT agent_id, SUBSTRING_INDEX(GROUP_CONCAT(database_profile_id ORDER BY created_at DESC), ',', 1) AS profile
    FROM agent_version WHERE database_profile_id IS NOT NULL GROUP BY agent_id
) v ON v.agent_id = a.id SET a.database_profile_id = v.profile;

ALTER TABLE agent_version DROP FOREIGN KEY fk_agent_version_database_profile;
ALTER TABLE agent_version DROP COLUMN database_profile_id;

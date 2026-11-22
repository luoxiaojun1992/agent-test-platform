-- Binds a Dataset Revision to the Target MySQL instances it was validated on (docs/architecture.md
-- section 16.5). A revision may be bound to one to eight Targets. Every binding is written in the publish
-- transaction, and none is added later. ordinal keeps the order given in the generation request: ordinal 0
-- is the Target the Dataset Critic and Test Case generation read from. The execution schema is named
-- dataset_<lowercase revision id> on every Target and is stored once, in dataset_revision.execution_database.

CREATE TABLE dataset_revision_target (
    dataset_revision_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    target_mysql_id     CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    ordinal             TINYINT UNSIGNED NOT NULL,
    created_at          TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (dataset_revision_id, target_mysql_id),
    UNIQUE KEY uq_dataset_revision_target_ordinal (dataset_revision_id, ordinal),
    KEY ix_dataset_revision_target_target (target_mysql_id),
    CONSTRAINT fk_dataset_revision_target_revision FOREIGN KEY (dataset_revision_id) REFERENCES dataset_revision (id),
    CONSTRAINT fk_dataset_revision_target_target FOREIGN KEY (target_mysql_id) REFERENCES target_mysql (id),
    CONSTRAINT ck_dataset_revision_target_ordinal CHECK (ordinal < 8)
) ENGINE=InnoDB;

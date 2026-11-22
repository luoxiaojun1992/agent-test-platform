-- Registered Target MySQL instances (docs/architecture.md section 16.5). The administrator password is
-- not stored here: admin_password_secret_name names a row of the secret table, entered through the
-- Credentials page. ca_certificate_pem is public material kept in Platform MySQL, optional, and used only
-- for VERIFY_IDENTITY. config_sha256 digests the request so an Idempotency-Key replay can be recognized.

CREATE TABLE target_mysql (
    id                          CHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    name                        VARCHAR(63) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    host                        VARCHAR(255) NOT NULL,
    port                        INT UNSIGNED NOT NULL,
    admin_username              VARCHAR(32) NOT NULL,
    admin_password_secret_name  VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tls_mode                    VARCHAR(32) NOT NULL,
    ca_certificate_pem          TEXT NULL,
    config_sha256               CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    idempotency_key             VARCHAR(128) NOT NULL,
    created_at                  TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    UNIQUE KEY uq_target_mysql_name (name),
    UNIQUE KEY uq_target_mysql_endpoint (host, port),
    UNIQUE KEY uq_target_mysql_idempotency (idempotency_key),
    KEY ix_target_mysql_page (created_at, id),
    CONSTRAINT fk_target_mysql_secret FOREIGN KEY (admin_password_secret_name) REFERENCES secret (name),
    CONSTRAINT ck_target_mysql_tls CHECK (tls_mode IN ('DISABLED_LOCAL_ONLY', 'VERIFY_IDENTITY'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

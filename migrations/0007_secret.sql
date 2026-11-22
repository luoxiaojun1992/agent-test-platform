-- Test-environment credentials entered through the Web UI or PUT /api/v1/secrets/{name}
-- (docs/technical/06-sandbox-and-security.md section 8.1). ciphertext is AES-256-GCM output (the
-- 16-byte GCM tag is appended) of at most 4096 plaintext bytes. key_id names the keyring entry that
-- sealed the value, so a key rotation can find every value still under an old key. The name is bound
-- as additional authenticated data. No API returns a plaintext value.

CREATE TABLE secret (
    name        VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    key_id      VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    nonce       BINARY(12) NOT NULL,
    ciphertext  VARBINARY(4112) NOT NULL,
    created_at  TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at  TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (name),
    KEY ix_secret_key_id (key_id),
    CONSTRAINT ck_secret_name CHECK (name REGEXP '^[a-z0-9][a-z0-9-]{0,127}$')
) ENGINE=InnoDB;

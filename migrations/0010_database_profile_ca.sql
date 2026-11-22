-- Optional public CA certificate of a Database Profile, used only with tls_mode VERIFY_IDENTITY. The certificate
-- is kept in Platform MySQL with the profile, not in the environment (docs/architecture.md section 16.4). When it
-- is NULL, the system root certificates verify the server.

ALTER TABLE database_profile
    ADD COLUMN ca_certificate_pem TEXT NULL AFTER tls_mode;

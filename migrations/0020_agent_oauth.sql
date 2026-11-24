-- OAuth 2.0 client credentials authentication for the Target Agent (docs/technical/08 section 4).
-- When token_url is set, the platform obtains a short-lived access token through the client
-- credentials grant instead of using the static target_bearer_token_ref.

ALTER TABLE agent
    ADD COLUMN client_id VARCHAR(255) NULL AFTER target_bearer_token_ref,
    ADD COLUMN client_secret_ref VARCHAR(512) NULL AFTER client_id,
    ADD COLUMN token_url VARCHAR(2048) NULL AFTER client_secret_ref;

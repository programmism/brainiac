-- 0023_oauth_credentials — per-source OAuth credential store (#246). Connectors
-- that use expiring OAuth access tokens (gdrive, gmail) break when the token
-- lapses; storing the refresh token + client details lets the app mint a fresh
-- access token itself. One row per source type.
--
-- Secrets (access/refresh token, client secret) are encrypted at rest with the
-- same app-level cipher as chunk text (#377/#403) when ENCRYPTION_KEY is set;
-- plaintext otherwise (opt-in, unchanged default). The initial refresh token comes
-- from an interactive OAuth consent flow the operator runs (out of scope here) and
-- is loaded with `kb oauth set`. Additive — nothing uses it until seeded, and the
-- <TYPE>_TOKEN env path keeps working when this table is empty.

CREATE TABLE oauth_credentials (
    source_type   text PRIMARY KEY,
    access_token  text,
    refresh_token text,
    expiry        timestamptz,
    token_url     text,
    client_id     text,
    client_secret text,
    updated_at    timestamptz NOT NULL DEFAULT now()
);

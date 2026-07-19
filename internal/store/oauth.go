package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// OAuthCredential is a per-source OAuth credential (#246): the current access
// token (+ expiry) and enough to refresh it. Secrets are encrypted at rest via the
// chunk-text cipher when a key is configured.
type OAuthCredential struct {
	SourceType   string
	AccessToken  string
	RefreshToken string
	Expiry       time.Time
	TokenURL     string
	ClientID     string
	ClientSecret string
}

// UpsertOAuthCredential stores (or replaces) a source's credential. The token
// secrets are encrypted at rest.
func UpsertOAuthCredential(ctx context.Context, db DBTX, c OAuthCredential) error {
	access, err := encryptText(c.AccessToken)
	if err != nil {
		return err
	}
	refresh, err := encryptText(c.RefreshToken)
	if err != nil {
		return err
	}
	secret, err := encryptText(c.ClientSecret)
	if err != nil {
		return err
	}
	_, err = db.Exec(ctx, `
		INSERT INTO oauth_credentials (source_type, access_token, refresh_token, expiry, token_url, client_id, client_secret, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, now())
		ON CONFLICT (source_type) DO UPDATE SET
			access_token = EXCLUDED.access_token, refresh_token = EXCLUDED.refresh_token,
			expiry = EXCLUDED.expiry, token_url = EXCLUDED.token_url,
			client_id = EXCLUDED.client_id, client_secret = EXCLUDED.client_secret,
			updated_at = now()`,
		c.SourceType, nullStr(access), nullStr(refresh), nullTime(c.Expiry), nullStr(c.TokenURL),
		nullStr(c.ClientID), nullStr(secret))
	return err
}

// UpdateOAuthAccessToken persists a freshly-refreshed access token + expiry (#246).
func UpdateOAuthAccessToken(ctx context.Context, db DBTX, sourceType, accessToken string, expiry time.Time) error {
	enc, err := encryptText(accessToken)
	if err != nil {
		return err
	}
	_, err = db.Exec(ctx,
		`UPDATE oauth_credentials SET access_token = $2, expiry = $3, updated_at = now() WHERE source_type = $1`,
		sourceType, nullStr(enc), nullTime(expiry))
	return err
}

// GetOAuthCredential returns a source's stored credential (decrypted), or (nil, nil)
// when none is stored.
func GetOAuthCredential(ctx context.Context, db DBTX, sourceType string) (*OAuthCredential, error) {
	var (
		c                       OAuthCredential
		access, refresh, secret *string
		tokenURL, clientID      *string
		expiry                  *time.Time
	)
	c.SourceType = sourceType
	err := db.QueryRow(ctx,
		`SELECT access_token, refresh_token, expiry, token_url, client_id, client_secret
		 FROM oauth_credentials WHERE source_type = $1`, sourceType).
		Scan(&access, &refresh, &expiry, &tokenURL, &clientID, &secret)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.AccessToken = deref(access)
	c.RefreshToken = deref(refresh)
	c.ClientSecret = deref(secret)
	c.TokenURL = deref(tokenURL)
	c.ClientID = deref(clientID)
	if expiry != nil {
		c.Expiry = *expiry
	}
	for _, p := range []*string{&c.AccessToken, &c.RefreshToken, &c.ClientSecret} {
		if err := decryptInto(p); err != nil {
			return nil, err
		}
	}
	return &c, nil
}

package core

import (
	"context"
	"net/http"
	"time"

	"github.com/programmism/brainiac/internal/oauth"
	"github.com/programmism/brainiac/internal/store"
)

// oauthRefreshSkew renews a token a little before it actually expires.
const oauthRefreshSkew = 60 * time.Second

// ResolveSourceToken returns the access token to use for a connector (#246). If a
// credential is stored for the source it is used — refreshed first when expired (or
// about to) — and the refreshed token is persisted. When nothing is stored it falls
// back to the caller's token (the <TYPE>_TOKEN env value), so the env path keeps
// working unchanged. Best-effort: a refresh failure falls back to the stored (maybe
// stale) or env token rather than breaking the import.
func (c *Core) ResolveSourceToken(ctx context.Context, sourceType, fallback string) (string, error) {
	cred, err := store.GetOAuthCredential(ctx, c.pool, sourceType)
	if err != nil {
		return "", err
	}
	if cred == nil {
		return fallback, nil // no stored credential → env token
	}
	fresh := cred.AccessToken != "" && !cred.Expiry.IsZero() && time.Now().Before(cred.Expiry.Add(-oauthRefreshSkew))
	if fresh {
		return cred.AccessToken, nil
	}
	// Needs (or might need) a refresh.
	if cred.RefreshToken == "" || cred.TokenURL == "" {
		if cred.AccessToken != "" {
			return cred.AccessToken, nil // can't refresh; use what we have
		}
		return fallback, nil
	}
	access, expiry, err := oauth.Refresh(ctx, http.DefaultClient, cred.TokenURL, cred.RefreshToken, cred.ClientID, cred.ClientSecret)
	if err != nil {
		if cred.AccessToken != "" {
			return cred.AccessToken, nil // refresh failed; fall back to the stored token
		}
		return "", err
	}
	_ = store.UpdateOAuthAccessToken(ctx, c.pool, sourceType, access, expiry) // best-effort persist
	return access, nil
}

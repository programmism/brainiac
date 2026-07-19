package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/programmism/brainiac/internal/store"
)

// TestResolveSourceToken covers the token resolver (#246): env fallback when no
// credential is stored, the stored token while fresh, and an auto-refresh (persisted)
// when it's expired.
func TestResolveSourceToken(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "TRUNCATE oauth_credentials"); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	// No stored credential → the env fallback token.
	if tok, err := c.ResolveSourceToken(ctx, "gmail", "env-tok"); err != nil || tok != "env-tok" {
		t.Fatalf("fallback = (%q, %v), want env-tok", tok, err)
	}

	// A stored, still-valid access token is used as-is.
	if err := store.UpsertOAuthCredential(ctx, pool, store.OAuthCredential{
		SourceType: "gmail", AccessToken: "stored-tok", Expiry: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("upsert valid: %v", err)
	}
	if tok, err := c.ResolveSourceToken(ctx, "gmail", "env-tok"); err != nil || tok != "stored-tok" {
		t.Fatalf("valid stored = (%q, %v), want stored-tok", tok, err)
	}

	// An expired token with refresh details → auto-refresh + persist.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"refreshed-tok","expires_in":3600}`))
	}))
	defer srv.Close()
	if err := store.UpsertOAuthCredential(ctx, pool, store.OAuthCredential{
		SourceType: "gmail", AccessToken: "old-tok", RefreshToken: "ref",
		Expiry: time.Now().Add(-time.Hour), TokenURL: srv.URL, ClientID: "c", ClientSecret: "s",
	}); err != nil {
		t.Fatalf("upsert expired: %v", err)
	}
	tok, err := c.ResolveSourceToken(ctx, "gmail", "env-tok")
	if err != nil || tok != "refreshed-tok" {
		t.Fatalf("refreshed = (%q, %v), want refreshed-tok", tok, err)
	}
	// The refreshed token was persisted.
	got, _ := store.GetOAuthCredential(ctx, pool, "gmail")
	if got == nil || got.AccessToken != "refreshed-tok" || got.Expiry.Before(time.Now()) {
		t.Fatalf("refreshed token not persisted: %+v", got)
	}
}

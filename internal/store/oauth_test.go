package store

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestOAuthCredentialStore round-trips a credential and a refreshed access token
// through the store (#246).
func TestOAuthCredentialStore(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping DB-backed oauth store test")
	}
	ctx := context.Background()
	pool, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := pool.Exec(ctx, "TRUNCATE oauth_credentials"); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	// Missing → (nil, nil).
	if got, err := GetOAuthCredential(ctx, pool, "gmail"); err != nil || got != nil {
		t.Fatalf("missing = (%v, %v), want (nil, nil)", got, err)
	}

	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	in := OAuthCredential{
		SourceType: "gmail", AccessToken: "acc-1", RefreshToken: "ref-1",
		Expiry: exp, TokenURL: "https://oauth2.googleapis.com/token",
		ClientID: "client-1", ClientSecret: "secret-1",
	}
	if err := UpsertOAuthCredential(ctx, pool, in); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := GetOAuthCredential(ctx, pool, "gmail")
	if err != nil || got == nil {
		t.Fatalf("get: %v (nil=%v)", err, got == nil)
	}
	if got.AccessToken != "acc-1" || got.RefreshToken != "ref-1" || got.ClientSecret != "secret-1" ||
		got.ClientID != "client-1" || got.TokenURL != in.TokenURL || !got.Expiry.Equal(exp) {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	// A refreshed access token replaces just the token + expiry.
	exp2 := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	if err := UpdateOAuthAccessToken(ctx, pool, "gmail", "acc-2", exp2); err != nil {
		t.Fatalf("update access: %v", err)
	}
	got, _ = GetOAuthCredential(ctx, pool, "gmail")
	if got.AccessToken != "acc-2" || !got.Expiry.Equal(exp2) || got.RefreshToken != "ref-1" {
		t.Fatalf("after refresh: %+v", got)
	}
}

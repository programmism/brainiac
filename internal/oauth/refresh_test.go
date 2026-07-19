package oauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRefresh(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method %s", r.Method)
		}
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "rt-123" {
			t.Errorf("bad form: %v", r.Form)
		}
		if r.Form.Get("client_id") != "cid" || r.Form.Get("client_secret") != "sec" {
			t.Errorf("client creds not sent: %v", r.Form)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"fresh-token","expires_in":3600}`))
	}))
	defer srv.Close()

	at, exp, err := Refresh(context.Background(), srv.Client(), srv.URL, "rt-123", "cid", "sec")
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if at != "fresh-token" {
		t.Fatalf("access token = %q, want fresh-token", at)
	}
	if exp.Before(time.Now().Add(59*time.Minute)) || exp.After(time.Now().Add(61*time.Minute)) {
		t.Fatalf("expiry = %v, want ~1h out", exp)
	}
}

func TestRefreshError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
	}))
	defer srv.Close()
	if _, _, err := Refresh(context.Background(), srv.Client(), srv.URL, "rt", "", ""); err == nil {
		t.Fatal("expected error on 400")
	}
}

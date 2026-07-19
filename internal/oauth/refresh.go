// Package oauth performs the OAuth 2.0 refresh_token grant (#246): exchange a
// stored refresh token for a fresh access token, so a connector's expiring token
// renews itself. Raw HTTP, no SDK — in keeping with the minimal-dependency stance.
// Obtaining the initial refresh token (the interactive consent flow) is out of
// scope and done by the operator.
package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"` // seconds
}

// Refresh exchanges refreshToken at tokenURL for a new access token and its expiry.
// clientSecret may be empty for public clients.
func Refresh(ctx context.Context, client *http.Client, tokenURL, refreshToken, clientID, clientSecret string) (string, time.Time, error) {
	if client == nil {
		client = http.DefaultClient
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	if clientID != "" {
		form.Set("client_id", clientID)
	}
	if clientSecret != "" {
		form.Set("client_secret", clientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("oauth refresh request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		return "", time.Time{}, fmt.Errorf("oauth refresh: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var tr tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", time.Time{}, fmt.Errorf("decode token response: %w", err)
	}
	if tr.AccessToken == "" {
		return "", time.Time{}, fmt.Errorf("oauth refresh: empty access_token in response")
	}
	var expiry time.Time
	if tr.ExpiresIn > 0 {
		expiry = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	}
	return tr.AccessToken, expiry, nil
}

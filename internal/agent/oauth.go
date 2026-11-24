package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// TokenResponse is the OAuth 2.0 token endpoint response.
type TokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
}

// FetchClientCredentialsToken performs the OAuth 2.0 client credentials grant against the Target's
// token endpoint and returns the access token. It is used when a Target Agent requires machine-to-
// machine authentication instead of a static bearer token (08 section 4).
func FetchClientCredentialsToken(ctx context.Context, httpClient HTTPDoer, tokenURL, clientID, clientSecret string) (string, error) {
	if tokenURL == "" || clientID == "" || clientSecret == "" {
		return "", fmt.Errorf("token URL, client ID and client secret are required")
	}
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("create token request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := httpClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("call token endpoint: %w", err)
	}
	defer response.Body.Close()
	body, err := readLimited(response.Body, maxResponseBytes)
	if err != nil {
		return "", err
	}
	if response.StatusCode != http.StatusOK {
		message := strings.TrimSpace(string(body))
		if len(message) > 2_000 {
			message = message[:2_000]
		}
		return "", &HTTPError{StatusCode: response.StatusCode, Body: message}
	}
	var token TokenResponse
	if err := json.Unmarshal(body, &token); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if token.AccessToken == "" {
		return "", fmt.Errorf("token response has no access_token")
	}
	return token.AccessToken, nil
}

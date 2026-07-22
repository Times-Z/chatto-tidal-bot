// Package tidal implements Tidal OAuth2 device authorization flow for
// authenticating with a Tidal HiFi Plus account.
package tidal

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

const (
	// deviceAuthURL is the Tidal OAuth2 device authorization endpoint.
	deviceAuthURL = "https://auth.tidal.com/v1/oauth2/device_authorization"
	// tokenURL is the Tidal OAuth2 token exchange endpoint.
	tokenURL = "https://auth.tidal.com/v1/oauth2/token"

	// defaultEncodedClient is the base64-encoded "client_id;client_secret"
	// used by go-tiddl and other open-source Tidal clients.
	defaultEncodedClient = "NE4zbjZRMXg5NUxMNUs3cDtvS09YZkpXMzcxY1g2eGFaMFB5aGdHTkJkTkxsQlpkNEFLS1lvdWdNamlrPQ=="
)

func defaultCredentials() (clientID, clientSecret string) {
	decoded, err := base64.StdEncoding.DecodeString(defaultEncodedClient)
	if err != nil {
		panic("tidal: failed to decode default client credentials: " + err.Error())
	}
	parts := strings.SplitN(string(decoded), ";", 2)
	return parts[0], parts[1]
}

// loadSavedToken reads a previously saved OAuth2 token from disk.
func loadSavedToken(tokenPath string) *oauth2.Token {
	data, err := os.ReadFile(tokenPath)
	if err != nil {
		return nil
	}
	var token oauth2.Token
	if err := json.Unmarshal(data, &token); err != nil {
		return nil
	}
	return &token
}

// saveToken persists an OAuth2 token to disk for reuse on subsequent launches.
func saveToken(tokenPath string, token *oauth2.Token) {
	data, err := json.MarshalIndent(token, "", "  ")
	if err != nil {
		slog.Warn("failed to marshal tidal token", "error", err)
		return
	}
	if err := os.WriteFile(tokenPath, data, 0600); err != nil {
		slog.Warn("failed to save tidal token", "error", err)
	}
}

// tidalScopes defines the OAuth2 scopes requested during device authorization.
var tidalScopes = []string{"r_usr", "w_usr", "w_sub"}

// deviceAuth initiates a Tidal device authorization flow, printing a URL
// and code for the user to authorize in their browser, then polls for
// the resulting token.
func deviceAuth(ctx context.Context, tokenPath string) (*oauth2.Token, error) {
	clientID, clientSecret := defaultCredentials()

	cfg := &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Endpoint: oauth2.Endpoint{
			TokenURL:  tokenURL,
			AuthStyle: oauth2.AuthStyleInHeader,
		},
		Scopes: tidalScopes,
	}

	httpClient := &http.Client{}

	form := strings.NewReader("client_id=" + cfg.ClientID + "&scope=" + strings.Join(cfg.Scopes, "+"))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, deviceAuthURL, form)
	if err != nil {
		return nil, fmt.Errorf("create device auth request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("initiate device auth: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("device auth request failed (status %d): %s", resp.StatusCode, string(body))
	}

	var authResp struct {
		DeviceCode              string `json:"deviceCode"`
		UserCode                string `json:"userCode"`
		VerificationUri         string `json:"verificationUri"`
		VerificationUriComplete string `json:"verificationUriComplete"`
		ExpiresIn               int    `json:"expiresIn"`
		Interval                int    `json:"interval"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&authResp); err != nil {
		return nil, fmt.Errorf("decode device auth response: %w", err)
	}

	fmt.Printf("\n=== TIDAL DEVICE AUTH ===\n")
	fmt.Printf("1. Open: %s\n", authResp.VerificationUriComplete)
	fmt.Printf("2. Enter code: %s\n", authResp.UserCode)
	fmt.Printf("==========================\n\n")

	time.Sleep(2 * time.Second)

	token, err := pollDeviceAuth(ctx, httpClient, cfg, authResp.DeviceCode, authResp.Interval)
	if err != nil {
		return nil, err
	}

	saveToken(tokenPath, token)
	return token, nil
}

// pollDeviceAuth polls the Tidal token endpoint at the specified interval
// until the user completes device authorization or the code expires.
func pollDeviceAuth(ctx context.Context, httpClient *http.Client, cfg *oauth2.Config, deviceCode string, intervalSec int) (*oauth2.Token, error) {
	for {
		form := strings.NewReader("client_id=" + cfg.ClientID + "&device_code=" + deviceCode + "&grant_type=urn:ietf:params:oauth:grant-type:device_code")
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Endpoint.TokenURL, form)
		if err != nil {
			return nil, fmt.Errorf("create token request: %w", err)
		}
		req.SetBasicAuth(cfg.ClientID, cfg.ClientSecret)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("poll token: %w", err)
		}

		if resp.StatusCode == 200 {
			var tokenResp struct {
				AccessToken  string `json:"access_token"`
				RefreshToken string `json:"refresh_token"`
				ExpiresIn    int    `json:"expires_in"`
				TokenType    string `json:"token_type"`
				Scope        string `json:"scope"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
				resp.Body.Close()
				return nil, fmt.Errorf("decode token response: %w", err)
			}
			resp.Body.Close()

			expiry := time.Time{}
			if tokenResp.ExpiresIn > 0 {
				expiry = time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
			}

			return &oauth2.Token{
				AccessToken:  tokenResp.AccessToken,
				RefreshToken: tokenResp.RefreshToken,
				TokenType:    tokenResp.TokenType,
				Expiry:       expiry,
			}, nil
		}

		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var errResp struct {
			Error            string `json:"error"`
			ErrorDescription string `json:"error_description"`
		}
		json.Unmarshal(body, &errResp)

		switch errResp.Error {
		case "authorization_pending":
			time.Sleep(time.Duration(intervalSec) * time.Second)
			continue
		case "expired_token", "invalid_grant":
			return nil, fmt.Errorf("device code expired")
		default:
			if errResp.Error != "" {
				return nil, fmt.Errorf("%s (%s)", errResp.Error, errResp.ErrorDescription)
			}
			return nil, fmt.Errorf("unexpected response (status %d): %s", resp.StatusCode, string(body))
		}
	}
}

package room

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// AdmissionRequest is the request payload for POST /v1/rooms/admission.
type AdmissionRequest struct {
	Room   string `json:"room"`
	Tenant string `json:"tenant,omitempty"`
}

// AdmissionResponse is the response from POST /v1/rooms/admission.
type AdmissionResponse struct {
	RoomToken  string `json:"room_token"`
	RoomName   string `json:"room_name"`
	Identity   string `json:"identity"`
	Assertion  string `json:"assertion"`
	ServerURL  string `json:"server_url,omitempty"`
	ExpiresAt  string `json:"expires_at"`
	CanPublish bool   `json:"can_publish"`
}

type authErrorResponse struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// DefaultHTTPTimeout bounds HTTP calls to the auth service.
const DefaultHTTPTimeout = 10 * time.Second

// Login performs a password grant to the auth service's /v1/token endpoint.
func Login(ctx context.Context, authURL, username, password, tenant string) (string, error) {
	if authURL == "" {
		return "", errors.New("auth URL is required")
	}
	endpoint := strings.TrimRight(authURL, "/") + "/v1/token"

	payload := map[string]string{
		"grant_type": "password",
		"username":   username,
		"password":   password,
	}
	if tenant != "" {
		payload["tenant"] = tenant
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal login request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", fmt.Errorf("create login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: DefaultHTTPTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("login request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read login response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var authErr authErrorResponse
		if json.Unmarshal(respBody, &authErr) == nil && authErr.Error != "" {
			return "", fmt.Errorf("login failed (%d): %s - %s", resp.StatusCode, authErr.Error, authErr.ErrorDescription)
		}
		return "", fmt.Errorf("login failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(respBody, &tokenResp); err != nil {
		return "", fmt.Errorf("unmarshal login response: %w", err)
	}
	if tokenResp.AccessToken == "" {
		return "", errors.New("login response missing access_token")
	}
	return tokenResp.AccessToken, nil
}

// RequestAdmission requests a scoped room token and participant assertion from
// POST /v1/rooms/admission on the auth service.
func RequestAdmission(ctx context.Context, authURL, token, tenant, room string) (*AdmissionResponse, error) {
	if authURL == "" {
		return nil, errors.New("auth URL is required")
	}
	if token == "" {
		return nil, errors.New("bearer token is required for room admission")
	}
	endpoint := strings.TrimRight(authURL, "/") + "/v1/rooms/admission"

	reqPayload := AdmissionRequest{
		Room:   room,
		Tenant: tenant,
	}
	bodyBytes, err := json.Marshal(reqPayload)
	if err != nil {
		return nil, fmt.Errorf("marshal admission request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create admission request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: DefaultHTTPTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("admission request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read admission response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var authErr authErrorResponse
		if json.Unmarshal(respBody, &authErr) == nil && authErr.Error != "" {
			return nil, fmt.Errorf("room admission denied (%d): %s - %s", resp.StatusCode, authErr.Error, authErr.ErrorDescription)
		}
		return nil, fmt.Errorf("room admission failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	var admissionResp AdmissionResponse
	if err := json.Unmarshal(respBody, &admissionResp); err != nil {
		return nil, fmt.Errorf("unmarshal admission response: %w", err)
	}
	return &admissionResp, nil
}

// ResolveCredentials acquires room credentials either by calling the central auth
// service (the production pattern) or minting locally with LIVEKIT_API_SECRET
// (explicit development mode).
func (cfg Config) ResolveCredentials(ctx context.Context, roomName, identity string) (serverURL string, token string, effectiveRoom string, canPublish bool, err error) {
	if cfg.RoomToken != "" {
		serverURL = cfg.URL
		return serverURL, cfg.RoomToken, roomName, true, nil
	}

	// 1. Auth service room admission
	if cfg.AuthURL != "" || cfg.AuthToken != "" || (cfg.Username != "" && cfg.Password != "") {
		authToken := cfg.AuthToken
		authURL := cfg.AuthURL
		if authURL == "" {
			authURL = "http://localhost:8081"
		}

		if authToken == "" && cfg.Username != "" && cfg.Password != "" {
			slog.InfoContext(ctx, "authenticating to auth service", "url", authURL, "username", cfg.Username)
			tok, err := Login(ctx, authURL, cfg.Username, cfg.Password, cfg.Tenant)
			if err != nil {
				return "", "", "", false, fmt.Errorf("auth login: %w", err)
			}
			authToken = tok
		}

		if authToken == "" {
			return "", "", "", false, errors.New("auth room admission requires either AuthToken or Username/Password")
		}

		slog.InfoContext(ctx, "requesting room admission", "auth_url", authURL, "room", roomName, "tenant", cfg.Tenant)
		adm, err := RequestAdmission(ctx, authURL, authToken, cfg.Tenant, roomName)
		if err != nil {
			return "", "", "", false, fmt.Errorf("room admission: %w", err)
		}

		serverURL = adm.ServerURL
		if serverURL == "" {
			serverURL = cfg.URL
		}
		slog.InfoContext(ctx, "room admission granted",
			"room_name", adm.RoomName,
			"identity", adm.Identity,
			"server_url", serverURL,
			"can_publish", adm.CanPublish,
			"expires_at", adm.ExpiresAt,
		)
		return serverURL, adm.RoomToken, adm.RoomName, adm.CanPublish, nil
	}

	// 2. Local development minting
	if cfg.APISecret != "" {
		slog.WarnContext(ctx, "using local token minting with LIVEKIT_API_SECRET (development mode; production uses auth room admission)")
		tok, err := mintToken(cfg, identity, roomName, time.Hour)
		if err != nil {
			return "", "", "", false, fmt.Errorf("local mint token: %w", err)
		}
		return cfg.URL, tok, roomName, true, nil
	}

	return "", "", "", false, errors.New("no room credentials: set -auth-url / -token (or AUTH_URL / AUTH_TOKEN) for auth admission, or set LIVEKIT_API_SECRET for local dev mode")
}

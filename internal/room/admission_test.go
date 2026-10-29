package room

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequestAdmission_Success(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/rooms/admission" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Fatalf("unexpected auth header: %s", r.Header.Get("Authorization"))
		}
		var req AdmissionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Room != "standup" {
			t.Fatalf("unexpected room: %s", req.Room)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(AdmissionResponse{
			RoomToken:  "mock-room-jwt",
			RoomName:   "ten_acme__standup",
			Identity:   "usr_123",
			Assertion:  "mock-assertion-jwt",
			ServerURL:  "ws://livekit.example:7880",
			CanPublish: true,
		})
	}))
	defer ts.Close()

	ctx := context.Background()
	adm, err := RequestAdmission(ctx, ts.URL, "test-token", "ten_acme", "standup")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if adm.RoomToken != "mock-room-jwt" {
		t.Errorf("expected room_token 'mock-room-jwt', got %q", adm.RoomToken)
	}
	if adm.RoomName != "ten_acme__standup" {
		t.Errorf("expected room_name 'ten_acme__standup', got %q", adm.RoomName)
	}
	if adm.ServerURL != "ws://livekit.example:7880" {
		t.Errorf("expected server_url 'ws://livekit.example:7880', got %q", adm.ServerURL)
	}
	if !adm.CanPublish {
		t.Errorf("expected can_publish true")
	}
}

func TestRequestAdmission_Denied(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(authErrorResponse{
			Error:            "forbidden",
			ErrorDescription: "room:standup#join not permitted",
		})
	}))
	defer ts.Close()

	ctx := context.Background()
	_, err := RequestAdmission(ctx, ts.URL, "bad-token", "ten_acme", "standup")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestLogin_Success(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/token" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode login: %v", err)
		}
		if payload["username"] != "alice" || payload["password"] != "secret123" {
			t.Fatalf("unexpected credentials: %v", payload)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "token-xyz",
		})
	}))
	defer ts.Close()

	ctx := context.Background()
	tok, err := Login(ctx, ts.URL, "alice", "secret123", "ten_acme")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != "token-xyz" {
		t.Errorf("expected token 'token-xyz', got %q", tok)
	}
}

func TestResolveCredentials_AuthService(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(AdmissionResponse{
			RoomToken:  "token-from-auth",
			RoomName:   "ten_test__demo",
			ServerURL:  "ws://livekit.from.auth:7880",
			CanPublish: true,
		})
	}))
	defer ts.Close()

	cfg := Config{
		AuthURL:   ts.URL,
		AuthToken: "valid-bearer",
	}

	serverURL, token, effectiveRoom, canPublish, err := cfg.ResolveCredentials(context.Background(), "demo", "client-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if serverURL != "ws://livekit.from.auth:7880" {
		t.Errorf("expected serverURL from auth, got %q", serverURL)
	}
	if token != "token-from-auth" {
		t.Errorf("expected token from auth, got %q", token)
	}
	if effectiveRoom != "ten_test__demo" {
		t.Errorf("expected effectiveRoom 'ten_test__demo', got %q", effectiveRoom)
	}
	if !canPublish {
		t.Errorf("expected canPublish true")
	}
}

func TestResolveCredentials_LocalDevMode(t *testing.T) {
	cfg := Config{
		URL:       "ws://localhost:8890",
		APIKey:    "devkey",
		APISecret: "secret-must-be-configured-for-minting",
	}

	serverURL, token, effectiveRoom, canPublish, err := cfg.ResolveCredentials(context.Background(), "local-room", "dev-client")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if serverURL != "ws://localhost:8890" {
		t.Errorf("expected serverURL 'ws://localhost:8890', got %q", serverURL)
	}
	if token == "" {
		t.Errorf("expected non-empty minted token")
	}
	if effectiveRoom != "local-room" {
		t.Errorf("expected effectiveRoom 'local-room', got %q", effectiveRoom)
	}
	if !canPublish {
		t.Errorf("expected canPublish true")
	}
}

func TestResolveCredentials_NoCredentialsError(t *testing.T) {
	cfg := Config{
		URL: "ws://localhost:8890",
	}

	_, _, _, _, err := cfg.ResolveCredentials(context.Background(), "room", "client")
	if err == nil {
		t.Fatal("expected error when no credentials provided, got nil")
	}
}

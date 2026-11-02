package room

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

type memoryStore struct {
	secrets map[string]string
}

func (m *memoryStore) Get(k string) (string, error) { return m.secrets[k], nil }
func (m *memoryStore) Set(k, v string) error        { m.secrets[k] = v; return nil }
func (m *memoryStore) Delete(k string) error        { delete(m.secrets, k); return nil }

func useMemoryStore(t *testing.T) *memoryStore {
	t.Helper()
	store := &memoryStore{secrets: map[string]string{}}
	previous := SecretStore
	SecretStore = store
	t.Cleanup(func() { SecretStore = previous })
	return store
}

func writeTokens(w http.ResponseWriter, access, refresh string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"access_token": access, "refresh_token": refresh})
}

func TestSessionAccessToken_RefreshesAndSavesRotatedSecret(t *testing.T) {
	store := useMemoryStore(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostForm.Get("grant_type") != "refresh_token" || r.PostForm.Get("refresh_token") != "ses.old" {
			t.Errorf("form = %v", r.PostForm)
		}
		if r.PostForm.Get("tenant") != "ten_acme" {
			t.Errorf("tenant = %q", r.PostForm.Get("tenant"))
		}
		writeTokens(w, "access-1", "ses.new")
	}))
	defer ts.Close()
	store.secrets[ts.URL] = "ses.old"

	token, err := sessionAccessToken(context.Background(), Config{AuthURL: ts.URL, Tenant: "ten_acme"})
	if err != nil {
		t.Fatal(err)
	}
	if token != "access-1" || store.secrets[ts.URL] != "ses.new" {
		t.Errorf("token = %q, stored = %q", token, store.secrets[ts.URL])
	}
}

func TestSessionAccessToken_KeepsSecretWhenServiceIsDown(t *testing.T) {
	store := useMemoryStore(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer ts.Close()
	store.secrets[ts.URL] = "ses.old"

	if _, err := sessionAccessToken(context.Background(), Config{AuthURL: ts.URL}); err == nil {
		t.Fatal("expected an error")
	}
	if store.secrets[ts.URL] != "ses.old" {
		t.Error("an outage cleared the session")
	}
}

// fakeAuthServer plays the auth service: authorize redirects straight to the
// loopback listener, as it would for a user already signed in.
func fakeAuthServer(t *testing.T) *httptest.Server {
	t.Helper()
	var challenge string
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/authorize":
			q := r.URL.Query()
			if q.Get("client_id") != OAuthClientID || q.Get("code_challenge_method") != "S256" {
				t.Errorf("authorize query = %v", q)
			}
			challenge = q.Get("code_challenge")
			target := q.Get("redirect_uri") + "?" + url.Values{"code": {"the-code"}, "state": {q.Get("state")}}.Encode()
			http.Redirect(w, r, target, http.StatusFound)
		case "/v1/token":
			_ = r.ParseForm()
			if r.PostForm.Get("code") != "the-code" || pkceChallenge(r.PostForm.Get("code_verifier")) != challenge {
				t.Errorf("token form = %v", r.PostForm)
			}
			if !strings.HasPrefix(r.PostForm.Get("redirect_uri"), "http://127.0.0.1:") {
				t.Errorf("redirect_uri = %q", r.PostForm.Get("redirect_uri"))
			}
			writeTokens(w, "access-2", "ses.first")
		case "/v1/revoke":
			_ = r.ParseForm()
			if r.PostForm.Get("refresh_token") != "ses.first" {
				t.Errorf("revoke form = %v", r.PostForm)
			}
		}
	}))
}

func withBrowser(t *testing.T, open func(string)) {
	t.Helper()
	previous := openBrowser
	openBrowser = func(target string) error {
		go open(target)
		return nil
	}
	t.Cleanup(func() { openBrowser = previous })
}

func TestSessionAccessToken_BrowserSignInWhenSessionEnded(t *testing.T) {
	store := useMemoryStore(t)
	refused := false
	fake := fakeAuthServer(t)
	defer fake.Close()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/token" {
			_ = r.ParseForm()
			if r.PostForm.Get("grant_type") == "refresh_token" {
				refused = true
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(authErrorResponse{Error: "invalid_grant"})
				return
			}
		}
		fake.Config.Handler.ServeHTTP(w, r)
	}))
	defer ts.Close()
	store.secrets[ts.URL] = "ses.dead"
	withBrowser(t, func(target string) {
		resp, err := http.Get(target)
		if err != nil {
			t.Errorf("browser: %v", err)
			return
		}
		resp.Body.Close()
	})

	token, err := sessionAccessToken(context.Background(), Config{AuthURL: ts.URL})
	if err != nil {
		t.Fatal(err)
	}
	if !refused || token != "access-2" || store.secrets[ts.URL] != "ses.first" {
		t.Errorf("refused = %v, token = %q, stored = %q", refused, token, store.secrets[ts.URL])
	}
}

func TestBrowserSignIn_RejectsForgedState(t *testing.T) {
	useMemoryStore(t)
	done := make(chan error, 1)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path == "/v1/authorize" {
			forged, err := http.Get(q.Get("redirect_uri") + "?code=stolen&state=wrong")
			if err != nil {
				done <- err
				return
			}
			forged.Body.Close()
			if forged.StatusCode != http.StatusBadRequest {
				done <- errors.New("forged callback accepted")
				return
			}
			done <- nil
			target := q.Get("redirect_uri") + "?" + url.Values{"error": {"access_denied"}, "state": {q.Get("state")}}.Encode()
			http.Redirect(w, r, target, http.StatusFound)
		}
	}))
	defer fake.Close()
	withBrowser(t, func(target string) {
		resp, err := http.Get(target)
		if err == nil {
			resp.Body.Close()
		}
	})

	_, err := browserSignIn(context.Background(), Config{AuthURL: fake.URL})
	if err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("err = %v", err)
	}
	if err := <-done; err != nil {
		t.Error(err)
	}
}

func TestLogout(t *testing.T) {
	store := useMemoryStore(t)
	ts := fakeAuthServer(t)
	defer ts.Close()
	store.secrets[ts.URL] = "ses.first"

	if err := Logout(context.Background(), ts.URL); err != nil {
		t.Fatal(err)
	}
	if store.secrets[ts.URL] != "" {
		t.Error("secret kept after logout")
	}
}

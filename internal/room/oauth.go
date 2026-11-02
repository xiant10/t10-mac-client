package room

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/zalando/go-keyring"
)

const (
	// OAuthClientID is this client's entry in the auth service's client list.
	OAuthClientID = "t10-mac-client"

	keychainService = "t10-client"
	browserTimeout  = 5 * time.Minute
)

// errSessionEnded means the auth service refused the stored session secret, so
// the user must sign in again. Any other refresh failure keeps the secret.
var errSessionEnded = errors.New("session ended")

// SecretStore keeps the session secret between runs. It is replaced in tests.
var SecretStore secretStore = keychain{}

type secretStore interface {
	Get(authURL string) (string, error)
	Set(authURL, secret string) error
	Delete(authURL string) error
}

// keychain stores one session secret per auth service in the macOS Keychain.
type keychain struct{}

func (keychain) Get(authURL string) (string, error) {
	secret, err := keyring.Get(keychainService, authURL)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", nil
	}
	return secret, err
}

func (keychain) Set(authURL, secret string) error {
	return keyring.Set(keychainService, authURL, secret)
}

func (keychain) Delete(authURL string) error {
	err := keyring.Delete(keychainService, authURL)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}

// openBrowser opens the system browser. It is replaced in tests.
var openBrowser = func(target string) error {
	return exec.Command("open", target).Run()
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

// sessionAccessToken returns an access token from the stored session, or from
// a browser sign-in if there is none. The new secret is saved at once, because
// the old one stops working as soon as the server sees the new one in use.
func sessionAccessToken(ctx context.Context, cfg Config) (string, error) {
	stored, err := SecretStore.Get(cfg.AuthURL)
	if err != nil {
		return "", fmt.Errorf("read keychain: %w", err)
	}
	if stored != "" {
		tokens, err := postToken(ctx, cfg.AuthURL, url.Values{
			"grant_type":    {"refresh_token"},
			"client_id":     {OAuthClientID},
			"refresh_token": {stored},
			"tenant":        {cfg.Tenant},
		})
		switch {
		case err == nil:
			return saveTokens(cfg.AuthURL, tokens)
		case errors.Is(err, errSessionEnded):
			slog.InfoContext(ctx, "stored session ended, signing in again")
			if err := SecretStore.Delete(cfg.AuthURL); err != nil {
				return "", fmt.Errorf("clear keychain: %w", err)
			}
		default:
			return "", err
		}
	}

	tokens, err := browserSignIn(ctx, cfg)
	if err != nil {
		return "", err
	}
	return saveTokens(cfg.AuthURL, tokens)
}

func saveTokens(authURL string, tokens tokenResponse) (string, error) {
	if tokens.RefreshToken != "" {
		if err := SecretStore.Set(authURL, tokens.RefreshToken); err != nil {
			return "", fmt.Errorf("write keychain: %w", err)
		}
	}
	return tokens.AccessToken, nil
}

// browserSignIn runs the code flow with PKCE: the system browser signs the user
// in, and a one-shot listener on a loopback port receives the code. This client
// never sees a password.
func browserSignIn(ctx context.Context, cfg Config) (tokenResponse, error) {
	verifier, err := randomURLSafe(32)
	if err != nil {
		return tokenResponse{}, err
	}
	state, err := randomURLSafe(16)
	if err != nil {
		return tokenResponse{}, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return tokenResponse{}, fmt.Errorf("open loopback listener: %w", err)
	}
	defer listener.Close()
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", listener.Addr().(*net.TCPAddr).Port)

	type callback struct {
		code string
		err  error
	}
	results := make(chan callback, 1)
	server := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			if r.URL.Path != "/callback" || q.Get("state") != state {
				http.Error(w, "unexpected request", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			var result callback
			if authErr := q.Get("error"); authErr != "" {
				result.err = fmt.Errorf("sign-in failed: %s", authErr)
				fmt.Fprintln(w, "Sign-in failed. You can close this window.")
			} else {
				result.code = q.Get("code")
				fmt.Fprintln(w, "Signed in. You can close this window.")
			}
			select {
			case results <- result:
			default:
			}
		}),
	}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()

	target := strings.TrimRight(cfg.AuthURL, "/") + "/v1/authorize?" + url.Values{
		"response_type":         {"code"},
		"client_id":             {OAuthClientID},
		"redirect_uri":          {redirectURI},
		"code_challenge":        {pkceChallenge(verifier)},
		"code_challenge_method": {"S256"},
		"state":                 {state},
	}.Encode()
	fmt.Fprintln(os.Stderr, "Sign in at:", target)
	if !cfg.NoBrowser {
		if err := openBrowser(target); err != nil {
			slog.WarnContext(ctx, "could not open a browser", "err", err)
		}
	}

	ctx, cancel := context.WithTimeout(ctx, browserTimeout)
	defer cancel()
	select {
	case result := <-results:
		if result.err != nil {
			return tokenResponse{}, result.err
		}
		return postToken(ctx, cfg.AuthURL, url.Values{
			"grant_type":    {"authorization_code"},
			"client_id":     {OAuthClientID},
			"code":          {result.code},
			"redirect_uri":  {redirectURI},
			"code_verifier": {verifier},
			"tenant":        {cfg.Tenant},
		})
	case <-ctx.Done():
		return tokenResponse{}, fmt.Errorf("waiting for browser sign-in: %w", ctx.Err())
	}
}

// postToken calls /v1/token. A refused code or secret returns errSessionEnded.
func postToken(ctx context.Context, authURL string, form url.Values) (tokenResponse, error) {
	endpoint := strings.TrimRight(authURL, "/") + "/v1/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, fmt.Errorf("create token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: DefaultHTTPTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return tokenResponse{}, fmt.Errorf("token request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return tokenResponse{}, fmt.Errorf("read token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var authErr authErrorResponse
		if json.Unmarshal(body, &authErr) == nil && authErr.Error != "" {
			if authErr.Error == "invalid_grant" {
				return tokenResponse{}, fmt.Errorf("%w: %s", errSessionEnded, authErr.ErrorDescription)
			}
			return tokenResponse{}, fmt.Errorf("token request failed (%d): %s - %s", resp.StatusCode, authErr.Error, authErr.ErrorDescription)
		}
		return tokenResponse{}, fmt.Errorf("token request failed with status %d", resp.StatusCode)
	}
	var tokens tokenResponse
	if err := json.Unmarshal(body, &tokens); err != nil {
		return tokenResponse{}, fmt.Errorf("unmarshal token response: %w", err)
	}
	if tokens.AccessToken == "" {
		return tokenResponse{}, errors.New("token response missing access_token")
	}
	return tokens, nil
}

// Logout ends the stored session on the auth service and forgets it here. The
// local copy goes even if the server cannot be reached.
func Logout(ctx context.Context, authURL string) error {
	stored, err := SecretStore.Get(authURL)
	if err != nil {
		return fmt.Errorf("read keychain: %w", err)
	}
	if stored == "" {
		return nil
	}
	var revokeErr error
	endpoint := strings.TrimRight(authURL, "/") + "/v1/revoke"
	form := url.Values{"client_id": {OAuthClientID}, "refresh_token": {stored}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("create revoke request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := &http.Client{Timeout: DefaultHTTPTimeout}
	resp, err := client.Do(req)
	if err != nil {
		revokeErr = fmt.Errorf("revoke request failed: %w", err)
	} else {
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			revokeErr = fmt.Errorf("revoke failed with status %d", resp.StatusCode)
		}
	}
	if err := SecretStore.Delete(authURL); err != nil {
		return fmt.Errorf("clear keychain: %w", err)
	}
	return revokeErr
}

func randomURLSafe(size int) (string, error) {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

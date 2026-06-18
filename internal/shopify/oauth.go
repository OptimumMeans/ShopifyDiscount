package shopify

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"time"
)

// AuthResult is the outcome of a successful OAuth handshake.
type AuthResult struct {
	AccessToken string
	Scope       string
}

// RunOAuth performs the Shopify OAuth authorization-code grant for a single
// shop: it starts a localhost callback server, opens the merchant's browser to
// the authorize screen, verifies the signed redirect, and exchanges the code
// for a (permanent, offline) Admin API access token.
//
// The app must list http://localhost:<port>/auth/callback as an allowed
// redirection URL, and must declare the requested scopes.
func RunOAuth(ctx context.Context, shop, clientID, clientSecret, scopes string, port int) (*AuthResult, error) {
	if shop == "" || clientID == "" || clientSecret == "" {
		return nil, fmt.Errorf("shop, client id, and client secret are all required")
	}
	nonce, err := randomState()
	if err != nil {
		return nil, err
	}
	redirectURI := fmt.Sprintf("http://localhost:%d/auth/callback", port)

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, fmt.Errorf("cannot listen on port %d (close anything using it, or pass -port): %w", port, err)
	}

	type result struct {
		code string
		err  error
	}
	done := make(chan result, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		code := q.Get("code")
		if code == "" {
			// Not the OAuth redirect we're waiting for (favicon, prefetch, or an
			// early bounce). Ignore it and keep the server alive.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		// HMAC (signed with our client secret) authenticates the response; this is
		// the meaningful check on a localhost loopback. State is best-effort.
		if !validHMAC(q, clientSecret) {
			http.Error(w, "HMAC validation failed", http.StatusBadRequest)
			done <- result{err: fmt.Errorf("HMAC validation failed (is the client secret correct?)")}
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><meta charset=utf-8><title>Connected</title>
<body style="font:16px system-ui;padding:48px;text-align:center">
<h2>✅ Connected to Shopify</h2><p>You can close this tab and return to the terminal.</p></body>`)
		done <- result{code: code}
	})

	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	defer srv.Close()

	authorizeURL := fmt.Sprintf(
		"https://%s/admin/oauth/authorize?client_id=%s&scope=%s&redirect_uri=%s&state=%s",
		shop, url.QueryEscape(clientID), url.QueryEscape(scopes),
		url.QueryEscape(redirectURI), url.QueryEscape(nonce))

	fmt.Println("Opening your browser to authorize the app…")
	fmt.Println("If it doesn't open, paste this URL into your browser:")
	fmt.Println("  " + authorizeURL)
	_ = openBrowser(authorizeURL)

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-done:
		if res.err != nil {
			return nil, res.err
		}
		return exchangeCode(ctx, shop, clientID, clientSecret, res.code)
	}
}

// exchangeCode trades the authorization code for an access token.
func exchangeCode(ctx context.Context, shop, clientID, clientSecret, code string) (*AuthResult, error) {
	body, _ := json.Marshal(map[string]string{
		"client_id":     clientID,
		"client_secret": clientSecret,
		"code":          code,
	})
	endpoint := fmt.Sprintf("https://%s/admin/oauth/access_token", shop)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
		Scope       string `json:"scope"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decoding token response (HTTP %d): %w", resp.StatusCode, err)
	}
	if out.AccessToken == "" {
		if out.Error != "" {
			return nil, fmt.Errorf("token exchange failed: %s — %s", out.Error, out.Description)
		}
		return nil, fmt.Errorf("token exchange failed (HTTP %d)", resp.StatusCode)
	}
	return &AuthResult{AccessToken: out.AccessToken, Scope: out.Scope}, nil
}

// validHMAC verifies Shopify's signature on the OAuth callback query.
func validHMAC(q url.Values, secret string) bool {
	given := q.Get("hmac")
	if given == "" {
		return false
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		if k == "hmac" || k == "signature" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+q.Get(k))
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strings.Join(parts, "&")))
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(given))
}

func randomState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// openBrowser best-effort opens a URL in the default browser.
func openBrowser(u string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("cmd", "/c", "start", "", u).Start()
	case "darwin":
		return exec.Command("open", u).Start()
	default:
		return exec.Command("xdg-open", u).Start()
	}
}

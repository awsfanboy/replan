// Package auth signs an attendee in with AWS Builder ID using the OAuth 2.0
// authorization code flow with PKCE, as the AWS Events API requires.
//
// Constraints from the API that shape this package:
//   - the app must run on the attendee's machine and listen at /callback on one
//     of the reserved loopback ports 8484-8489 (exact match, no wildcards);
//   - the client ID is public and shared; there is no client secret;
//   - access tokens last 60 minutes, refresh tokens 30 days, and a refresh may
//     rotate the refresh token;
//   - tokens go only to api.awsevents.com and are stored where only the user
//     can read them.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	AuthorizeURL = "https://oauth.awsevents.com/oauth2/authorize"
	TokenURL     = "https://oauth.awsevents.com/oauth2/token"
	RevokeURL    = "https://oauth.awsevents.com/oauth2/revoke"
	ClientID     = "7vmom55m1qstvq8i71ph127bfq"
	Scope        = "openid email events/access"
	FirstPort    = 8484
	LastPort     = 8489
)

// Tokens is what we persist. The ID token is kept only to show who is signed in.
type Tokens struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	IDToken      string    `json:"id_token,omitempty"`
	Expiry       time.Time `json:"expiry"`
}

// Store persists tokens in a file readable only by the current user.
type Store struct {
	Path string
}

func DefaultStore() (*Store, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	return &Store{Path: filepath.Join(dir, "replan", "tokens.json")}, nil
}

func (s *Store) Load() (*Tokens, error) {
	b, err := os.ReadFile(s.Path)
	if err != nil {
		return nil, err
	}
	var t Tokens
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *Store) Save(t *Tokens) error {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.Path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.Path)
}

func (s *Store) Delete() error {
	err := os.Remove(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Source is an api.TokenSource backed by a Store. It refreshes ahead of expiry
// and when the API reports a 401.
type Source struct {
	Store *Store
	HTTP  *http.Client

	mu  sync.Mutex
	tok *Tokens
}

var ErrNotSignedIn = errors.New("not signed in: run `replan login`")

func (s *Source) Token(ctx context.Context, force bool) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tok == nil {
		t, err := s.Store.Load()
		if err != nil {
			return "", ErrNotSignedIn
		}
		s.tok = t
	}
	if !force && time.Until(s.tok.Expiry) > 2*time.Minute {
		return s.tok.AccessToken, nil
	}
	if s.tok.RefreshToken == "" {
		return "", ErrNotSignedIn
	}
	nt, err := refresh(ctx, s.httpClient(), s.tok.RefreshToken)
	if err != nil {
		return "", fmt.Errorf("refreshing sign-in (run `replan login` again): %w", err)
	}
	s.tok = nt
	if err := s.Store.Save(nt); err != nil {
		return "", err
	}
	return nt.AccessToken, nil
}

func (s *Source) httpClient() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int    `json:"expires_in"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

func postForm(ctx context.Context, hc *http.Client, endpoint string, form url.Values) (*tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var tr tokenResponse
	_ = json.NewDecoder(resp.Body).Decode(&tr)
	if resp.StatusCode != http.StatusOK {
		if tr.Error != "" {
			return nil, fmt.Errorf("%s: %s %s", endpoint, tr.Error, tr.ErrorDesc)
		}
		return nil, fmt.Errorf("%s: HTTP %d", endpoint, resp.StatusCode)
	}
	return &tr, nil
}

func refresh(ctx context.Context, hc *http.Client, refreshToken string) (*Tokens, error) {
	tr, err := postForm(ctx, hc, TokenURL, url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {ClientID},
		"refresh_token": {refreshToken},
	})
	if err != nil {
		return nil, err
	}
	t := toTokens(tr)
	if t.RefreshToken == "" {
		t.RefreshToken = refreshToken // not rotated
	}
	return t, nil
}

func toTokens(tr *tokenResponse) *Tokens {
	exp := tr.ExpiresIn
	if exp <= 0 {
		exp = 3600
	}
	return &Tokens{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		IDToken:      tr.IDToken,
		Expiry:       time.Now().Add(time.Duration(exp) * time.Second),
	}
}

// Login runs the interactive PKCE sign-in: it listens on the first free
// reserved port, opens the browser, and exchanges the returned code.
func Login(ctx context.Context, hc *http.Client, out func(string)) (*Tokens, error) {
	ln, port, err := listenReserved()
	if err != nil {
		return nil, err
	}
	defer ln.Close()
	// Both host forms are registered; use the IPv4 literal so the browser can't
	// resolve "localhost" to ::1 while we listen on 127.0.0.1.
	redirect := fmt.Sprintf("http://127.0.0.1:%d/callback", port)

	verifier := randomString(64)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	state := randomString(32)

	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {ClientID},
		"redirect_uri":          {redirect},
		"scope":                 {Scope},
		"identity_provider":     {"AWSBuilderID"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
	}
	authURL := AuthorizeURL + "?" + q.Encode()

	type result struct {
		code string
		err  error
	}
	done := make(chan result, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		qs := r.URL.Query()
		var res result
		switch {
		case qs.Get("error") != "":
			res.err = fmt.Errorf("sign-in failed: %s %s", qs.Get("error"), qs.Get("error_description"))
		case qs.Get("state") != state:
			res.err = errors.New("sign-in failed: state mismatch")
		case qs.Get("code") == "":
			res.err = errors.New("sign-in failed: no code returned")
		default:
			res.code = qs.Get("code")
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if res.err != nil {
			fmt.Fprintf(w, "<h2>replan: sign-in failed</h2><p>%s</p>", html.EscapeString(res.err.Error()))
		} else {
			fmt.Fprint(w, "<h2>replan: signed in ✅</h2><p>You can close this tab and return to your terminal.</p>")
		}
		select {
		case done <- res:
		default:
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	defer srv.Close()

	out("Opening your browser to sign in with AWS Builder ID...")
	out("If it does not open, visit:\n  " + authURL)
	_ = openBrowser(authURL)

	var res result
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(5 * time.Minute):
		return nil, errors.New("timed out waiting for sign-in")
	case res = <-done:
	}
	if res.err != nil {
		return nil, res.err
	}
	tr, err := postForm(ctx, hc, TokenURL, url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {ClientID},
		"redirect_uri":  {redirect},
		"code":          {res.code},
		"code_verifier": {verifier},
	})
	if err != nil {
		return nil, err
	}
	return toTokens(tr), nil
}

// Revoke revokes the refresh token. Already-issued access tokens stay valid
// until they expire, so callers must also discard them.
func Revoke(ctx context.Context, hc *http.Client, refreshToken string) error {
	_, err := postForm(ctx, hc, RevokeURL, url.Values{
		"client_id": {ClientID},
		"token":     {refreshToken},
	})
	return err
}

// Email extracts the email claim from the ID token for display. The token is
// not verified; it is only used to print who is signed in.
func (t *Tokens) Email() string {
	parts := strings.Split(t.IDToken, ".")
	if len(parts) != 3 {
		return ""
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var c struct {
		Email string `json:"email"`
	}
	_ = json.Unmarshal(b, &c)
	return c.Email
}

func listenReserved() (net.Listener, int, error) {
	for p := FirstPort; p <= LastPort; p++ {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err == nil {
			return ln, p, nil
		}
	}
	return nil, 0, fmt.Errorf("all reserved sign-in ports %d-%d are busy; free one and retry", FirstPort, LastPort)
}

func randomString(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)[:n]
}

func openBrowser(u string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", u).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start()
	default:
		return exec.Command("xdg-open", u).Start()
	}
}

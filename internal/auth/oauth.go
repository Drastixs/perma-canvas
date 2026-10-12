package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	MobileVerifyURL = "https://sso.canvaslms.com/api/v1/mobile_verify.json"
	// RedirectURI is the redirect the Canvas mobile apps register; the
	// authorization code arrives as ?code= on this URL.
	RedirectURI = "https://sso.canvaslms.com/canvas/login"
	appUA       = "candroid/8.6.1 (289)"
)

var (
	// ErrUnauthorized means Canvas rejected the access token.
	ErrUnauthorized = errors.New("canvas rejected access token")
	// ErrGrantRevoked means the refresh token no longer works and a fresh login is required.
	ErrGrantRevoked = errors.New("refresh token rejected; login required")
)

// OAuth talks to the Canvas OAuth endpoints used by the mobile apps.
type OAuth struct {
	HTTP            *http.Client
	MobileVerifyURL string
	// Scheme lets tests point at plain-HTTP fakes.
	Scheme string
}

func NewOAuth(client *http.Client) *OAuth {
	return &OAuth{HTTP: client, MobileVerifyURL: MobileVerifyURL, Scheme: "https"}
}

func (o *OAuth) base(domain string) string { return o.Scheme + "://" + domain }

// MobileCredentials fetches the Canvas mobile app's client id/secret for a domain.
func (o *OAuth) MobileCredentials(ctx context.Context, domain string) (id, secret string, err error) {
	u := o.MobileVerifyURL + "?" + url.Values{"domain": {domain}, "user_agent": {appUA}}.Encode()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	req.Header.Set("User-Agent", appUA)
	var out struct {
		Authorized   bool   `json:"authorized"`
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if err := o.doJSON(req, &out); err != nil {
		return "", "", fmt.Errorf("mobile_verify: %w", err)
	}
	if out.ClientID == "" || out.ClientSecret == "" {
		return "", "", fmt.Errorf("mobile_verify returned no credentials for %s", domain)
	}
	return out.ClientID, out.ClientSecret, nil
}

// AuthorizeURL is the URL a browser must visit to start the mobile OAuth flow.
func (o *OAuth) AuthorizeURL(domain, clientID, purpose string) string {
	state := make([]byte, 32)
	if _, err := rand.Read(state); err != nil {
		panic(err)
	}
	return o.base(domain) + "/login/oauth2/auth?" + url.Values{
		"state":                   {hex.EncodeToString(state)},
		"authentication_provider": {"saml"},
		"client_id":               {clientID},
		"response_type":           {"code"},
		"mobile":                  {"1"},
		"purpose":                 {purpose},
		"redirect_uri":            {RedirectURI},
	}.Encode()
}

// CodeFromInput accepts either a bare authorization code or the full redirect URL.
func CodeFromInput(in string) string {
	in = strings.TrimSpace(in)
	if u, err := url.Parse(in); err == nil && u.Scheme != "" {
		return u.Query().Get("code")
	}
	return in
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	User         User   `json:"user"`
}

func (o *OAuth) token(ctx context.Context, domain string, form url.Values) (*tokenResponse, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, o.base(domain)+"/login/oauth2/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", appUA)
	var out tokenResponse
	if err := o.doJSON(req, &out); err != nil {
		return nil, err
	}
	if out.AccessToken == "" {
		return nil, errors.New("token endpoint returned no access_token")
	}
	return &out, nil
}

// Exchange trades an authorization code for tokens.
func (o *OAuth) Exchange(ctx context.Context, domain, id, secret, code string) (*tokenResponse, error) {
	return o.token(ctx, domain, url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {id},
		"client_secret": {secret},
		"code":          {code},
	})
}

// Refresh obtains a new access token from the permanent refresh token.
func (o *OAuth) Refresh(ctx context.Context, domain, id, secret, refresh string) (*tokenResponse, error) {
	out, err := o.token(ctx, domain, url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {id},
		"client_secret": {secret},
		"refresh_token": {refresh},
	})
	var se *statusError
	if errors.As(err, &se) && (se.code == http.StatusBadRequest || se.code == http.StatusUnauthorized) {
		return nil, fmt.Errorf("%w: %v", ErrGrantRevoked, err)
	}
	return out, err
}

// SessionURL asks Canvas for a one-time URL that logs a browser into a web
// session (GET /login/session_token, the endpoint the mobile apps use to open
// web views). The URL embeds a token valid for ~30 seconds.
func (o *OAuth) SessionURL(ctx context.Context, domain, accessToken, returnTo string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		o.base(domain)+"/login/session_token?"+url.Values{"return_to": {returnTo}}.Encode(), nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("User-Agent", appUA)
	var out struct {
		SessionURL string `json:"session_url"`
	}
	err := o.doJSON(req, &out)
	var se *statusError
	if errors.As(err, &se) && se.code == http.StatusUnauthorized {
		return "", ErrUnauthorized
	}
	if err != nil {
		return "", fmt.Errorf("session_token: %w", err)
	}
	if out.SessionURL == "" {
		return "", errors.New("session_token returned no session_url")
	}
	return out.SessionURL, nil
}

type statusError struct {
	code int
	body string
}

func (e *statusError) Error() string { return fmt.Sprintf("Canvas HTTP %d", e.code) }

func (o *OAuth) doJSON(req *http.Request, out any) error {
	resp, err := o.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return &statusError{code: resp.StatusCode}
	}
	return json.Unmarshal(body, out)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func expiry(now time.Time, expiresIn int64) time.Time {
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	return now.Add(time.Duration(expiresIn) * time.Second)
}

package auth

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"time"
)

// ErrNoGrant means no tokens are stored yet.
var ErrNoGrant = errors.New("no Canvas grant stored; run login")

// Manager owns the stored grant: it refreshes access tokens on demand and mints
// web-session URLs from them.
type Manager struct {
	OAuth   *OAuth
	Store   *Store
	Domain  string
	Purpose string
	Now     func() time.Time

	mu      sync.Mutex
	tok     *Tokens
	loaded  bool
	lastErr error
	lastErrAt,
	lastSessionAt time.Time
	pending *pendingLogin
}

type pendingLogin struct {
	clientID, clientSecret, state string
	startedAt                     time.Time
}

func NewManager(o *OAuth, s *Store, domain, purpose string) *Manager {
	return &Manager{OAuth: o, Store: s, Domain: domain, Purpose: purpose, Now: time.Now}
}

// tokens returns the in-memory grant, loading it from disk once. Caller holds mu.
func (m *Manager) tokens() (*Tokens, error) {
	if !m.loaded {
		t, err := m.Store.Load()
		if err != nil {
			return nil, err
		}
		if t != nil && t.Domain != m.Domain {
			return nil, fmt.Errorf("stored grant is for %s, not %s", t.Domain, m.Domain)
		}
		m.tok, m.loaded = t, true
	}
	if m.tok == nil || m.tok.RefreshToken == "" {
		return nil, ErrNoGrant
	}
	return m.tok, nil
}

// accessToken returns a usable access token, refreshing when expired or forced. Caller holds mu.
func (m *Manager) accessToken(ctx context.Context, force bool) (string, error) {
	t, err := m.tokens()
	if err != nil {
		return "", err
	}
	now := m.Now()
	if !force && t.AccessToken != "" && now.Before(t.AccessExpiresAt.Add(-2*time.Minute)) {
		return t.AccessToken, nil
	}
	r, err := m.OAuth.Refresh(ctx, m.Domain, t.ClientID, t.ClientSecret, t.RefreshToken)
	if err != nil {
		return "", err
	}
	t.AccessToken, t.AccessExpiresAt, t.RefreshedAt = r.AccessToken, expiry(now, r.ExpiresIn), now
	if r.RefreshToken != "" {
		t.RefreshToken = r.RefreshToken
	}
	if r.User.ID != 0 {
		t.User = r.User
	}
	if err := m.Store.Save(t); err != nil {
		return "", fmt.Errorf("save refreshed token: %w", err)
	}
	return t.AccessToken, nil
}

// SessionURL mints a one-time Canvas web-login URL that lands on returnTo.
func (m *Manager) SessionURL(ctx context.Context, returnTo string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, err := m.sessionURL(ctx, returnTo)
	m.record(err)
	if err == nil {
		m.lastSessionAt = m.Now()
	}
	return u, err
}

func (m *Manager) sessionURL(ctx context.Context, returnTo string) (string, error) {
	at, err := m.accessToken(ctx, false)
	if err != nil {
		return "", err
	}
	u, err := m.OAuth.SessionURL(ctx, m.Domain, at, returnTo)
	if !errors.Is(err, ErrUnauthorized) {
		return u, err
	}
	if at, err = m.accessToken(ctx, true); err != nil {
		return "", err
	}
	return m.OAuth.SessionURL(ctx, m.Domain, at, returnTo)
}

// AccessToken returns a valid access token (refreshing if needed).
func (m *Manager) AccessToken(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	at, err := m.accessToken(ctx, false)
	m.record(err)
	return at, err
}

func (m *Manager) record(err error) {
	if err != nil {
		m.lastErr, m.lastErrAt = err, m.Now()
	} else {
		m.lastErr = nil
	}
}

// StartLogin fetches mobile client credentials and returns the URL the user
// must open to authorise a new grant. The credentials are kept for FinishLogin.
func (m *Manager) StartLogin(ctx context.Context) (string, error) {
	id, secret, err := m.OAuth.MobileCredentials(ctx, m.Domain)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	authorizeURL := m.OAuth.AuthorizeURL(m.Domain, id, m.Purpose)
	parsed, _ := url.Parse(authorizeURL)
	m.pending = &pendingLogin{clientID: id, clientSecret: secret, state: parsed.Query().Get("state"), startedAt: m.Now()}
	m.mu.Unlock()
	return authorizeURL, nil
}

// FinishLogin exchanges the authorization code from a StartLogin flow.
func (m *Manager) FinishLogin(ctx context.Context, codeOrURL string) (*Tokens, error) {
	m.mu.Lock()
	p := m.pending
	m.mu.Unlock()
	if p == nil {
		return nil, errors.New("no login in progress")
	}
	if m.Now().Sub(p.startedAt) > 5*time.Minute {
		return nil, errors.New("login expired")
	}
	if u, err := url.Parse(codeOrURL); err == nil && u.Scheme != "" && u.Query().Get("state") != p.state {
		return nil, errors.New("OAuth state mismatch")
	}
	code := CodeFromInput(codeOrURL)
	if code == "" {
		return nil, errors.New("no authorization code found in input")
	}
	r, err := m.OAuth.Exchange(ctx, m.Domain, p.clientID, p.clientSecret, code)
	if err != nil {
		return nil, fmt.Errorf("exchange code: %w", err)
	}
	now := m.Now()
	t := &Tokens{
		Domain: m.Domain, Purpose: m.Purpose,
		ClientID: p.clientID, ClientSecret: p.clientSecret,
		AccessToken: r.AccessToken, AccessExpiresAt: expiry(now, r.ExpiresIn),
		RefreshToken: r.RefreshToken, User: r.User,
		ObtainedAt: now, RefreshedAt: now,
	}
	if t.RefreshToken == "" {
		return nil, errors.New("token exchange returned no refresh_token")
	}
	if err := m.Store.Save(t); err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.tok, m.loaded, m.pending, m.lastErr = t, true, nil, nil
	m.mu.Unlock()
	return t, nil
}

// Status is a secret-free snapshot for health checks and the admin page.
type Status struct {
	HasGrant        bool      `json:"has_grant"`
	User            User      `json:"user"`
	Purpose         string    `json:"purpose"`
	ObtainedAt      time.Time `json:"obtained_at"`
	RefreshedAt     time.Time `json:"last_refreshed_at"`
	AccessExpiresAt time.Time `json:"access_expires_at"`
	LastSessionAt   time.Time `json:"last_session_at"`
	LastError       string    `json:"last_error,omitempty"`
	LastErrorAt     time.Time `json:"last_error_at"`
	NeedsLogin      bool      `json:"needs_login"`
	LoginPending    bool      `json:"login_pending"`
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, err := m.tokens()
	s := Status{LastSessionAt: m.lastSessionAt, LoginPending: m.pending != nil}
	if t != nil {
		s.HasGrant, s.User, s.Purpose = true, t.User, t.Purpose
		s.ObtainedAt, s.RefreshedAt, s.AccessExpiresAt = t.ObtainedAt, t.RefreshedAt, t.AccessExpiresAt
	}
	if err == nil {
		err = m.lastErr
	}
	if err != nil {
		s.LastError, s.LastErrorAt = err.Error(), m.lastErrAt
		s.NeedsLogin = errors.Is(err, ErrNoGrant) || errors.Is(err, ErrGrantRevoked)
	}
	return s
}

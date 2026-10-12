// Package proxy serves the original Canvas UI with a server-owned session.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

type SessionSource interface {
	SessionURL(context.Context, string) (string, error)
}

type Proxy struct {
	upstream, public *url.URL
	source           SessionSource
	client           *http.Client
	jar              http.CookieJar
	reverse          *httputil.ReverseProxy
	mu               sync.Mutex
	ready            bool
	generation       uint64
}

func New(upstream, public *url.URL, source SessionSource) *Proxy {
	jar, _ := cookiejar.New(nil)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 45 * time.Second
	p := &Proxy{upstream: upstream, public: public, source: source, jar: jar}
	p.client = &http.Client{Transport: transport, Jar: jar, Timeout: 45 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if req.URL.Host != upstream.Host || req.URL.Scheme != upstream.Scheme {
			return http.ErrUseLastResponse
		}
		if len(via) >= 8 {
			return errors.New("too many Canvas session redirects")
		}
		return nil
	}}
	p.reverse = &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(upstream)
			r.Out.Host = upstream.Host
			r.Out.Header.Del("Authorization")
			r.Out.Header.Del("Cookie")
			r.Out.Header.Del("Forwarded")
			r.Out.Header.Del("X-Forwarded-For")
			r.Out.Header.Del("X-Forwarded-Host")
			r.Out.Header.Del("X-Forwarded-Proto")
			for _, h := range []string{"Remote-User", "Remote-Email", "Remote-Groups", "Remote-Name"} {
				r.Out.Header.Del(h)
			}
			for _, h := range []string{"Origin", "Referer"} {
				r.Out.Header.Set(h, p.toUpstream(r.In.Header.Get(h)))
			}
			r.Out.Header.Set("Accept-Encoding", "identity")
			for _, c := range jar.Cookies(r.Out.URL) {
				r.Out.AddCookie(c)
			}
		},
		Transport:      transport,
		ModifyResponse: p.modifyResponse,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "Canvas upstream unavailable", http.StatusBadGateway)
		},
	}
	return p
}

func (p *Proxy) toPublic(s string) string {
	s = strings.ReplaceAll(s, p.upstream.String(), p.public.String())
	s = strings.ReplaceAll(s, "//"+p.upstream.Host, "//"+p.public.Host)
	return strings.ReplaceAll(s, strings.ReplaceAll(p.upstream.String(), "/", `\/`), strings.ReplaceAll(p.public.String(), "/", `\/`))
}
func (p *Proxy) toUpstream(s string) string {
	return strings.ReplaceAll(s, p.public.String(), p.upstream.String())
}

func (p *Proxy) ensureSession(ctx context.Context, observed uint64, force bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ready && (!force || p.generation != observed) {
		return nil
	}
	sessionURL, err := p.source.SessionURL(ctx, p.upstream.String()+"/")
	if err != nil {
		return err
	}
	u, err := url.Parse(sessionURL)
	if err != nil || u.Host != p.upstream.Host || u.Scheme != p.upstream.Scheme {
		return errors.New("unexpected Canvas session URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return errors.New("Canvas session request failed")
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 || p.loginRedirect(resp) {
		return errors.New("Canvas did not establish a session")
	}
	if len(p.jar.Cookies(p.upstream)) == 0 {
		return errors.New("Canvas returned no session cookies")
	}
	p.ready = true
	p.generation++
	return nil
}

func (p *Proxy) loginRedirect(resp *http.Response) bool {
	if resp.StatusCode < 300 || resp.StatusCode >= 400 {
		return false
	}
	u, err := resp.Request.URL.Parse(resp.Header.Get("Location"))
	if err != nil {
		return false
	}
	if u.Host != p.upstream.Host {
		return false
	}
	return u.Path == "/login" || strings.HasPrefix(u.Path, "/login/") && !strings.HasPrefix(u.Path, "/login/session_token")
}

type generationKey struct{}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Host != p.public.Host {
		http.Error(w, "Unexpected host", http.StatusMisdirectedRequest)
		return
	}
	if r.URL.Path == "/logout" || strings.HasPrefix(r.URL.Path, "/login") {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if r.URL.Query().Has("session_token") || r.URL.Query().Has("access_token") {
		http.Error(w, "Authentication parameters are managed by the server", http.StatusBadRequest)
		return
	}
	if err := p.ensureSession(r.Context(), 0, false); err != nil {
		http.Error(w, "Canvas login required or token refresh unavailable; run perma-canvas login", http.StatusServiceUnavailable)
		return
	}
	p.mu.Lock()
	gen := p.generation
	p.mu.Unlock()
	p.reverse.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), generationKey{}, gen)))
}

func (p *Proxy) modifyResponse(resp *http.Response) error {
	p.jar.SetCookies(resp.Request.URL, resp.Cookies())
	// Never replay a submission or upload. Safe reads get one session renewal.
	if p.loginRedirect(resp) || resp.StatusCode == http.StatusUnauthorized {
		gen, _ := resp.Request.Context().Value(generationKey{}).(uint64)
		if err := p.ensureSession(resp.Request.Context(), gen, true); err != nil {
			return errors.New("Canvas session renewal failed")
		}
		if (resp.Request.Method == http.MethodGet || resp.Request.Method == http.MethodHead) && resp.Request.ContentLength == 0 {
			resp.Body.Close()
			req := resp.Request.Clone(resp.Request.Context())
			req.RequestURI = ""
			req.Header = req.Header.Clone()
			req.Header.Del("Cookie")
			for _, c := range p.jar.Cookies(req.URL) {
				req.AddCookie(c)
			}
			// Do not follow normal redirects: they must be rewritten for the browser.
			client := *p.client
			client.Jar = nil
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			fresh, err := client.Do(req)
			if err != nil {
				return errors.New("Canvas retry failed")
			}
			*resp = *fresh
			p.jar.SetCookies(resp.Request.URL, resp.Cookies())
			if p.loginRedirect(resp) {
				resp.Body.Close()
				return errors.New("Canvas session renewal was rejected")
			}
		} else {
			resp.Body.Close()
			resp.StatusCode = http.StatusConflict
			resp.Status = "409 Conflict"
			resp.Header = make(http.Header)
			resp.Header.Set("Content-Type", "text/plain; charset=utf-8")
			msg := "Canvas session renewed. Reload the page before submitting again. Your request was not replayed."
			resp.Body = io.NopCloser(strings.NewReader(msg))
			resp.ContentLength = int64(len(msg))
		}
	}
	// Only expose the CSRF cookie. The upstream account session stays on the VM.
	cookies := p.jar.Cookies(resp.Request.URL)
	resp.Header.Del("Set-Cookie")
	for _, c := range cookies {
		if c.Name == "_csrf_token" {
			c.Domain = ""
			c.Path = "/"
			c.Secure = p.public.Scheme == "https"
			resp.Header.Add("Set-Cookie", c.String())
		}
	}
	for _, h := range []string{"Location", "Content-Location", "Link", "Content-Security-Policy", "Content-Security-Policy-Report-Only", "Refresh", "Access-Control-Allow-Origin"} {
		values := resp.Header.Values(h)
		resp.Header.Del(h)
		for _, v := range values {
			resp.Header.Add(h, p.toPublic(v))
		}
	}
	resp.Header.Set("Cache-Control", "private, no-store")
	ct := resp.Header.Get("Content-Type")
	textual := strings.Contains(ct, "text/html") || strings.Contains(ct, "javascript") || strings.Contains(ct, "application/json") || strings.Contains(ct, "text/css")
	if textual && resp.Request.Method != http.MethodHead && resp.StatusCode != http.StatusNoContent {
		if enc := resp.Header.Get("Content-Encoding"); enc != "" && enc != "identity" {
			return fmt.Errorf("unexpected upstream content encoding")
		}
		const limit = 32 << 20
		body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		resp.Body.Close()
		if err != nil {
			return err
		}
		if len(body) > limit {
			return errors.New("Canvas response too large to rewrite")
		}
		body = []byte(p.toPublic(string(body)))
		resp.Body = io.NopCloser(strings.NewReader(string(body)))
		resp.ContentLength = int64(len(body))
		resp.Header.Set("Content-Length", fmt.Sprint(len(body)))
		resp.Header.Del("ETag")
		resp.Header.Del("Content-MD5")
	}
	return nil
}

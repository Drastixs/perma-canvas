package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

type fakeSource struct {
	mu     sync.Mutex
	count  int
	origin string
}

func (s *fakeSource) SessionURL(context.Context, string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.count++
	return s.origin + "/bootstrap", nil
}
func TestSessionRecoveryAndRewrite(t *testing.T) {
	var mu sync.Mutex
	expired := false
	posts := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/bootstrap" {
			expired = false
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "secret", Path: "/"})
			http.SetCookie(w, &http.Cookie{Name: "_csrf_token", Value: "csrf", Path: "/"})
			return
		}
		if _, err := r.Cookie("session"); err != nil {
			t.Error("missing upstream session")
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("browser authorization leaked")
		}
		if r.Method == "POST" {
			posts++
		}
		if expired {
			http.Redirect(w, r, "/login/saml", 302)
			return
		}
		if r.Header.Get("Origin") != "" && r.Header.Get("Origin") != "http://"+r.Host {
			t.Error("origin not rewritten")
		}
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Location", "http://"+r.Host+"/courses")
		io.WriteString(w, `<a href="http://`+r.Host+`/courses">Canvas</a>`)
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	public, _ := url.Parse("https://canvas.example.test")
	source := &fakeSource{origin: upstream.URL}
	p := New(u, public, source)
	p.reverse.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		t.Logf("proxy error: %v", err)
		http.Error(w, err.Error(), 502)
	}
	request := func(method string) *httptest.ResponseRecorder {
		var body io.Reader
		if method == "POST" {
			body = strings.NewReader("submission")
		}
		r := httptest.NewRequest(method, "https://canvas.example.test/courses", body)
		r.Header.Set("Authorization", "Bearer browser-secret")
		r.Header.Set("Origin", public.String())
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		return w
	}
	w := request("GET")
	if w.Code != 200 || !strings.Contains(w.Body.String(), public.String()) || strings.Contains(w.Body.String(), upstream.URL) {
		t.Fatalf("bad rewritten response: %d %s", w.Code, w.Body.String())
	}
	if len(w.Result().Cookies()) != 1 || w.Result().Cookies()[0].Name != "_csrf_token" {
		t.Fatal("session cookie exposed or csrf absent")
	}
	mu.Lock()
	expired = true
	mu.Unlock()
	w = request("GET")
	if w.Code != 200 || source.count != 2 {
		t.Fatalf("read recovery failed: %d, sessions %d", w.Code, source.count)
	}
	mu.Lock()
	expired = true
	mu.Unlock()
	w = request("POST")
	if w.Code != 409 || posts != 1 {
		t.Fatalf("submission replayed or not rejected: %d posts %d", w.Code, posts)
	}
	r := httptest.NewRequest("GET", "https://attacker.test/", nil)
	w = httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != 421 {
		t.Fatal("host check failed")
	}
}

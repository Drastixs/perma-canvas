package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"
)

func TestRefreshIsSerializedAndPersisted(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		r.ParseForm()
		if r.Form.Get("refresh_token") != "refresh" {
			t.Error("wrong refresh token")
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "new", "refresh_token": "rotated", "expires_in": 3600})
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	store := NewStore(t.TempDir())
	err := store.Save(&Tokens{Domain: u.Host, RefreshToken: "refresh", AccessToken: "expired", AccessExpiresAt: time.Now().Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	o := NewOAuth(server.Client())
	o.Scheme = "http"
	m := NewManager(o, store, u.Host, "test")
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			at, err := m.AccessToken(context.Background())
			if err != nil || at != "new" {
				t.Errorf("refresh: %s %v", at, err)
			}
		}()
	}
	wg.Wait()
	if calls != 1 {
		t.Fatalf("refreshed %d times", calls)
	}
	saved, err := store.Load()
	if err != nil || saved.RefreshToken != "rotated" {
		t.Fatal("rotation not persisted")
	}
	info, _ := os.Stat(store.Path())
	if info.Mode().Perm() != 0600 {
		t.Fatal("unsafe permissions")
	}
}
func TestOAuthErrorsDoNotExposeSecrets(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401); w.Write([]byte("secret-token")) }))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	o := NewOAuth(server.Client())
	o.Scheme = "http"
	_, err := o.Refresh(context.Background(), u.Host, "id", "secret", "refresh")
	if err == nil || err.Error() != "refresh token rejected; login required: Canvas HTTP 401" {
		t.Fatalf("unexpected error %v", err)
	}
}

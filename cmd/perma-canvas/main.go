package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Drastixs/perma-canvas/internal/auth"
	"github.com/Drastixs/perma-canvas/internal/proxy"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
func run() error {
	domain := env("CANVAS_DOMAIN", "canvas.institution.example")
	upstream, err := url.Parse("https://" + domain)
	if err != nil || upstream.Host != domain || upstream.Path != "" || upstream.User != nil || strings.ContainsAny(domain, "?#") {
		return fmt.Errorf("CANVAS_DOMAIN must be a hostname")
	}
	public, err := url.Parse(env("PUBLIC_URL", "https://canvas.example.com"))
	if err != nil || public.Host == "" || public.User != nil || public.RawQuery != "" || public.Fragment != "" || (public.Path != "" && public.Path != "/") || (public.Scheme != "https" && public.Scheme != "http") {
		return fmt.Errorf("PUBLIC_URL must be an http(s) origin")
	}
	public.Path = ""
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	manager := auth.NewManager(auth.NewOAuth(client), auth.NewStore(env("DATA_DIR", "./data")), domain, "perma-canvas")
	mode := "serve"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	switch mode {
	case "login":
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		u, err := manager.StartLogin(ctx)
		if err != nil {
			return err
		}
		var code string
		if env("LOGIN_BROWSER", "playwright") == "manual" {
			fmt.Fprintln(os.Stderr, "Open this URL, then paste the final callback URL or authorization code:\n"+u)
			scanner := bufio.NewScanner(os.Stdin)
			if !scanner.Scan() {
				return fmt.Errorf("no callback received")
			}
			code = scanner.Text()
		} else {
			cmd := exec.CommandContext(ctx, env("PYTHON", "python3"), env("LOGIN_SCRIPT", "login/capture_code.py"))
			cmd.Env = append(os.Environ(), "CANVAS_AUTHORIZE_URL="+u)
			cmd.Stdin = os.Stdin
			cmd.Stderr = os.Stderr
			out, err := cmd.Output()
			if err != nil {
				return fmt.Errorf("login browser failed: %w", err)
			}
			var result struct {
				Code string `json:"code"`
			}
			if err = json.Unmarshal(out, &result); err != nil {
				return fmt.Errorf("invalid login helper output")
			}
			code = result.Code
		}
		t, err := manager.FinishLogin(ctx, code)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Canvas grant saved for %s. The browser has exited.\n", t.User.Name)
		return nil
	case "status":
		return json.NewEncoder(os.Stdout).Encode(manager.Status())
	case "serve":
		p := proxy.New(upstream, public, manager)
		mux := http.NewServeMux()
		mux.HandleFunc("GET /_perma/health", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(struct {
				HasGrant bool `json:"has_grant"`
			}{manager.Status().HasGrant})
		})
		mux.Handle("/", p)
		server := &http.Server{Addr: env("LISTEN_ADDR", "127.0.0.1:8080"), Handler: mux, ReadHeaderTimeout: 15 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 1 << 20}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		go func() {
			<-ctx.Done()
			c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_ = server.Shutdown(c)
		}()
		log.Printf("serving %s at %s", public, server.Addr)
		err = server.ListenAndServe()
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	default:
		return fmt.Errorf("usage: perma-canvas [serve|login|status]")
	}
}

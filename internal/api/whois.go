package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/rrhubenov/dune/internal/run"
)

type userKey struct{}

// UserFrom returns the authenticated Tailscale login, or "local" for requests
// over the unix socket (whose permissions already restrict it to root).
func UserFrom(ctx context.Context) string {
	if u, ok := ctx.Value(userKey{}).(string); ok {
		return u
	}
	return "local"
}

// WhoisAuth admits a tailnet request only if `tailscale whois` maps its
// source address to a login in allow. An empty allow list admits no one.
// Lookups are cached per source IP for a minute.
type WhoisAuth struct {
	Allow []string // Tailscale login names, e.g. "rrhubenov@github"

	mu    sync.Mutex
	cache map[string]whoisEntry
}

type whoisEntry struct {
	login string
	at    time.Time
}

const whoisTTL = time.Minute

func (a *WhoisAuth) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		login, err := a.Check(r.RemoteAddr)
		if err != nil {
			writeJSON(w, http.StatusForbidden, Error{Error: err.Error()})
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey{}, login)))
	})
}

// Check returns the Tailscale login behind a remote "ip:port" if it is
// allowed, and logs denials.
func (a *WhoisAuth) Check(remote string) (string, error) {
	login, err := a.lookup(remote)
	if err != nil {
		slog.Warn("whois failed", "remote", remote, "err", err)
		return "", errors.New("cannot identify Tailscale peer")
	}
	if !slices.Contains(a.Allow, login) {
		slog.Warn("denied", "remote", remote, "login", login)
		return "", errors.New("Tailscale user " + login + " is not allowed")
	}
	return login, nil
}

func (a *WhoisAuth) lookup(remote string) (string, error) {
	ip, _, err := net.SplitHostPort(remote)
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	e, ok := a.cache[ip]
	a.mu.Unlock()
	if ok && time.Since(e.at) < whoisTTL {
		return e.login, nil
	}
	out, err := run.Output("tailscale", "whois", "--json", ip)
	if err != nil {
		return "", err
	}
	var resp struct {
		UserProfile struct {
			LoginName string
		}
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		return "", err
	}
	a.mu.Lock()
	if a.cache == nil {
		a.cache = map[string]whoisEntry{}
	}
	a.cache[ip] = whoisEntry{resp.UserProfile.LoginName, time.Now()}
	a.mu.Unlock()
	return resp.UserProfile.LoginName, nil
}

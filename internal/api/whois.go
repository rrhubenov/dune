package api

import (
	"context"
	"encoding/json"
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
		login, err := a.lookup(r.RemoteAddr)
		if err != nil {
			slog.Warn("whois failed", "remote", r.RemoteAddr, "err", err)
			writeJSON(w, http.StatusForbidden, Error{"cannot identify Tailscale peer"})
			return
		}
		if !slices.Contains(a.Allow, login) {
			slog.Warn("denied", "remote", r.RemoteAddr, "login", login)
			writeJSON(w, http.StatusForbidden, Error{"Tailscale user " + login + " is not allowed"})
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey{}, login)))
	})
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

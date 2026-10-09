package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"
)

func TestImportRejectsPreparedBatchesAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	preparations, rejects := 0, 0
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.URL.Path == "/api/v1/commands/reject" {
			rejects++
			json.NewEncoder(w).Encode(commandResult{OK: true, Data: map[string]any{"rejected": true}})
			return
		}
		if r.Form.Get("dry_run") == "true" {
			writeCommandResult(w, map[string]any{"validated": true})
			return
		}
		preparations++
		if preparations == 2 {
			cancel() // Ctrl-C or an exhausted budget while preparing the second batch.
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writeCommandResult(w, map[string]any{"confirmation_token": "prepared-token"})
	})
	defer server.Close()
	c, _ := newClient(server.URL, "secret")
	batches := [][]byte{[]byte(`{"items":[{"asset_name":"One"}]}`), []byte(`{"items":[{"asset_name":"Two"}]}`)}
	if _, err := c.importResources(ctx, "assets", batches, false); err == nil || rejects != 1 {
		t.Fatalf("cancelled import did not release prepared batches: rejects=%d err=%v", rejects, err)
	}
}

func TestRetryAfterBeyondBudgetReturnsRateLimitImmediately(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	requests := 0
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	c, _ := newClient(server.URL, "secret")
	started := time.Now()
	err := c.json(ctx, http.MethodGet, "/limited", nil, true, false, nil)
	var ae *apiError
	if !errors.As(err, &ae) || ae.Code != "rate_limited" || requests != 1 || time.Since(started) > time.Second {
		t.Fatalf("expected an immediate rate_limited error, got %v after %d requests in %s", err, requests, time.Since(started))
	}
}

func TestRefreshServiceUnavailableKeepsTokenReusable(t *testing.T) {
	for _, tc := range []struct {
		status  int
		pending bool
	}{{http.StatusServiceUnavailable, false}, {http.StatusBadGateway, true}} {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
		})
		s, unlock, err := openStore("refresh", true)
		if err != nil {
			t.Fatal(err)
		}
		value := session{Server: server.URL, TenantNumber: "12345678", Scope: agentScope,
			AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(-time.Minute)}
		_, err = refreshSessionTokens(t.Context(), s, value, io.Discard)
		stored, loadErr := s.load()
		unlock()
		var ae *apiError
		if !errors.As(err, &ae) || ae.Code != "server_unavailable" || loadErr != nil {
			t.Fatalf("HTTP %d: unexpected error %v (load %v)", tc.status, err, loadErr)
		}
		if stored.RefreshPending != tc.pending {
			t.Fatalf("HTTP %d: RefreshPending = %v, want %v", tc.status, stored.RefreshPending, tc.pending)
		}
	}
}

func TestLoginRevokesTokenAfterCancellation(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	revoked := make(chan string, 1)
	var origin string
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case apiPaths["oauth#revoke"]:
			r.ParseForm()
			revoked <- r.Form.Get("token")
		case apiPaths["oauth#device_authorization"]:
			json.NewEncoder(w).Encode(map[string]any{
				"device_code": "device-secret", "user_code": "ABCD-EFGH", "verification_uri": origin + "/settings",
				"expires_in": 60, "interval": 1,
			})
		case apiPaths["oauth#token"]:
			json.NewEncoder(w).Encode(map[string]any{
				"access_token": "access-token", "refresh_token": "refresh-token", "token_type": "Bearer",
				"expires_in": 3600, "scope": agentScope,
			})
		default:
			cancel() // The person interrupts while the CLI verifies the new connection.
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	origin = server.URL
	s, unlock, err := openStore("login", true)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := login(ctx, s, server.URL, io.Discard); err == nil || ctx.Err() == nil {
		t.Fatalf("expected the login to fail by interruption, got %v", err)
	}
	select {
	case token := <-revoked:
		if token != "refresh-token" {
			t.Fatalf("revoked %q", token)
		}
	default:
		t.Fatal("interrupted login left the refresh token active")
	}
}

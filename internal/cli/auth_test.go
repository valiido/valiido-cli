package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPairingRejectsForeignOriginAndCancels(t *testing.T) {
	for _, uri := range []string{"https://other.example/verify", "same"} {
		t.Run(uri, func(t *testing.T) {
			var origin string
			server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				verification := uri
				if verification == "same" {
					verification = origin + "/api/pairing"
				}
				json.NewEncoder(w).Encode(deviceResponse{DeviceCode: "device-secret", UserCode: "ABCD-1234", VerificationURI: verification, ExpiresIn: 600, Interval: 5})
			})

			origin = server.URL
			c, _ := newClient(origin, "")
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			var instructions bytes.Buffer
			_, err := c.pair(ctx, &instructions)
			if err == nil || strings.Contains(instructions.String(), "device-secret") || strings.Contains(instructions.String(), "other.example") {
				t.Fatal("unsafe pairing instructions or cancellation failure")
			}
		})
	}
}

func TestPairingUsesCompleteSameOriginLinkAndSendsMachineName(t *testing.T) {
	var origin string
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.Form.Get("agent_name") == "" {
			t.Fatal("device authorization omitted machine name")
		}
		json.NewEncoder(w).Encode(deviceResponse{
			DeviceCode: "device-secret", UserCode: "ABCD-1234",
			VerificationURI:         origin + "/settings?section=agents",
			VerificationURIComplete: origin + "/settings?section=agents&user_code=ABCD-1234&agent_name=workstation-7",
			ExpiresIn:               600, Interval: 5,
		})
	})

	origin = server.URL
	c, _ := newClient(origin, "")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	var instructions bytes.Buffer
	_, err := c.pair(ctx, &instructions)
	if err == nil || !strings.Contains(instructions.String(), "user_code=ABCD-1234") || !strings.Contains(instructions.String(), "agent_name=workstation-7") {
		t.Fatalf("complete pairing link was not shown: %v %q", err, instructions.String())
	}
}

func TestPairingRejectsForeignCompleteLink(t *testing.T) {
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(deviceResponse{
			DeviceCode: "device-secret", UserCode: "ABCD-1234",
			VerificationURI:         "http://" + r.Host + "/settings?section=agents",
			VerificationURIComplete: "https://other.example/settings?user_code=ABCD-1234",
			ExpiresIn:               600, Interval: 5,
		})
	})

	c, _ := newClient(server.URL, "")
	var instructions bytes.Buffer
	if _, err := c.pair(context.Background(), &instructions); err == nil || instructions.Len() != 0 {
		t.Fatal("accepted a foreign complete pairing link")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestPairingStopsWhenInstructionsCannotBeWritten(t *testing.T) {
	requests := 0
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		json.NewEncoder(w).Encode(deviceResponse{
			DeviceCode: "device-code", UserCode: "ABCD-1234",
			VerificationURI: "http://" + r.Host + "/settings", ExpiresIn: 60, Interval: 1,
		})
	})

	c, _ := newClient(server.URL, "")
	if _, err := c.pair(context.Background(), failingWriter{}); err == nil || requests != 1 {
		t.Fatalf("pairing continued after output failure: requests=%d err=%v", requests, err)
	}
}

func TestPairingCancellationReleasesProfile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ready := make(chan struct{})
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(deviceResponse{
			DeviceCode: "synthetic-device", UserCode: "ABCD-1234",
			VerificationURI: "http://" + r.Host + "/activate", ExpiresIn: 600, Interval: 10,
		})
		close(ready)
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan int, 1)
	go func() {
		result <- Run(ctx, []string{"auth", "login", "--profile", "pair", "--server", server.URL}, io.Discard, io.Discard)
	}()
	select {
	case <-ready:
		cancel()
	case <-time.After(3 * time.Second):
		t.Fatal("pairing never started")
	}
	select {
	case code := <-result:
		if code == 0 {
			t.Fatal("cancelled pairing succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pairing did not stop")
	}
	_, unlock, err := openStoreContext(t.Context(), "pair", true, 0)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}

func TestAuthenticatedRequestRefreshesBeforeExpiry(t *testing.T) {
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fresh" {
			t.Fatalf("request used stale access token: %q", r.Header.Get("Authorization"))
		}
		io.WriteString(w, `{}`)
	})

	c, _ := newClient(server.URL, "stale")
	c.refreshAt = time.Now().Add(-30 * time.Second)
	c.refresh = func(context.Context, bool) (string, time.Time, error) {
		return "fresh", time.Now().Add(15 * time.Minute), nil
	}
	if err := c.json(context.Background(), "GET", "/test", nil, true, false, nil); err != nil {
		t.Fatal(err)
	}
}

func TestAuthenticatedRequestRefreshesOnceAfterUnauthorized(t *testing.T) {
	requests := 0
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Authorization") != "Bearer fresh" {
			t.Fatalf("retry used wrong access token: %q", r.Header.Get("Authorization"))
		}
		io.WriteString(w, `{}`)
	})

	c, _ := newClient(server.URL, "stale")
	refreshes := 0
	c.refresh = func(context.Context, bool) (string, time.Time, error) {
		refreshes++
		return "fresh", time.Now().Add(time.Minute), nil
	}
	if err := c.json(context.Background(), "GET", "/test", nil, true, false, nil); err != nil {
		t.Fatal(err)
	}
	if requests != 2 || refreshes != 1 {
		t.Fatalf("requests=%d refreshes=%d, want 2 requests and 1 refresh", requests, refreshes)
	}
}

func TestRefreshStoredSessionReusesValidAccessToken(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, tt := range []struct {
		name       string
		staleToken string
		lifetime   time.Duration
		exchanges  int32
	}{
		{"outside refresh window", "current", 3 * time.Minute, 0},
		{"rotated while waiting", "previous", -time.Minute, 0},
		{"server reports slow down", "current", -time.Minute, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var exchanges atomic.Int32
			server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				exchanges.Add(1)
				w.WriteHeader(http.StatusBadRequest)
				io.WriteString(w, `{"error":"slow_down"}`)
			})

			s, unlock, err := openStore("shared", true)
			if err != nil {
				t.Fatal(err)
			}
			value := session{
				Server: server.URL, TenantNumber: "12345678", Scope: agentScope,
				AccessToken: "current", RefreshToken: "current-refresh", ExpiresAt: time.Now().Add(tt.lifetime),
			}
			err = s.save(value)
			unlock()
			if err != nil {
				t.Fatal(err)
			}

			previous := value
			previous.AccessToken = tt.staleToken
			got, err := refreshStoredSession(t.Context(), "shared", previous, false, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if got.AccessToken != value.AccessToken || got.RefreshToken != value.RefreshToken || !got.ExpiresAt.Equal(value.ExpiresAt) {
				t.Fatal("expected the stored credentials to be reused")
			}
			if count := exchanges.Load(); count != tt.exchanges {
				t.Fatalf("exchanges=%d, want %d", count, tt.exchanges)
			}
		})
	}
}

func TestCLIRefreshPreservesLegacyReadScopeAndLogout(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	refreshes, revokes := 0, 0
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			refreshes++
			r.ParseForm()
			if r.Form.Get("refresh_token") != "old-refresh" {
				t.Error("wrong refresh token")
			}
			json.NewEncoder(w).Encode(tokenResponse{AccessToken: "new-access", RefreshToken: "new-refresh", TokenType: "Bearer", Scope: legacyReadScope, ExpiresIn: 900})
		case "/oauth/revoke":
			revokes++
			r.ParseForm()
			if r.Form.Get("token") != "new-refresh" {
				t.Error("did not revoke latest refresh token")
			}
		case "/api/v1/me":
			if r.Header.Get("Authorization") != "Bearer new-access" {
				t.Error("did not use refreshed access token")
			}
			json.NewEncoder(w).Encode(identity{TenantNumber: "12345678", Scope: legacyReadScope, AgentName: "Agent"})
		case "/api/v1/commands":
			writeCommandResult(w, resource{Type: "documents", ID: 42, Version: resourceVersion(2), Label: "Test", Attributes: map[string]any{
				"title": "Test", "kind": "rich_text", "content_state": "text", "content_text": "Hello\x1b[31m\nWorld",
			}})
		default:
			http.NotFound(w, r)
		}
	})

	s, unlock, _ := openStore("default", true)
	if err := s.save(session{Server: server.URL, TenantNumber: "12345678", Scope: legacyReadScope, AccessToken: "old", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	unlock()
	var out, progress bytes.Buffer
	if code := Run(context.Background(), []string{"documents", "read", "42", "--text"}, &out, &progress); code != 0 {
		t.Fatalf("read failed: %s", out.String())
	}
	if refreshes != 1 || strings.ContainsRune(out.String(), '\x1b') || !strings.Contains(out.String(), "World") {
		t.Fatal("refresh or safe text rendering failed")
	}
	out.Reset()
	if code := Run(context.Background(), []string{"auth", "logout"}, &out, &progress); code != 0 || revokes != 1 {
		t.Fatalf("logout failed: %s", out.String())
	}
	if _, err := os.Stat(s.path()); !os.IsNotExist(err) {
		t.Fatal("logout did not remove credentials")
	}
}

func TestTokenEnvelopeHasSafetyBounds(t *testing.T) {
	valid := tokenResponse{AccessToken: "access_token", RefreshToken: "refresh-token", TokenType: "Bearer", Scope: agentScope, ExpiresIn: maxAccessSeconds}
	if err := validateToken(valid, agentScope); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*tokenResponse){
		func(token *tokenResponse) { token.AccessToken = strings.Repeat("a", maxTokenBytes+1) },
		func(token *tokenResponse) { token.RefreshToken = "contains a space" },
		func(token *tokenResponse) { token.ExpiresIn = maxAccessSeconds + 1 },
	} {
		invalid := valid
		mutate(&invalid)
		if validateToken(invalid, agentScope) == nil {
			t.Fatalf("accepted invalid token envelope: %#v", invalid)
		}
	}
}

func TestRefreshUnderLockUsesStoredPairAndRejectsAmbiguousReplay(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	current := session{
		Server: "http://127.0.0.1:1", TenantNumber: "12345678", Scope: agentScope,
		AccessToken: "current-access", RefreshToken: "current-refresh",
		ExpiresAt: time.Now().Add(-time.Hour), RefreshPending: true,
	}
	s, unlock, err := openStore("shared", true)
	if err != nil {
		t.Fatal(err)
	}
	err = s.save(current)
	unlock()
	if err != nil {
		t.Fatal(err)
	}

	for _, previous := range []session{
		{Server: current.Server, TenantNumber: current.TenantNumber, Scope: current.Scope,
			AccessToken: "old-access", RefreshToken: current.RefreshToken},
		{Server: current.Server, TenantNumber: current.TenantNumber, Scope: current.Scope,
			AccessToken: current.AccessToken, RefreshToken: "old-refresh"},
	} {
		got, err := refreshStoredSession(t.Context(), "shared", previous, true, io.Discard)
		if err != nil || got.AccessToken != current.AccessToken || got.RefreshToken != current.RefreshToken {
			t.Fatalf("failed to reuse the stored token pair: %v", err)
		}
	}
	_, err = refreshStoredSession(t.Context(), "shared", current, true, io.Discard)
	var ae *apiError
	if !errors.As(err, &ae) || ae.Code != "auth" {
		t.Fatalf("expected reauthentication without another exchange, got %v", err)
	}
}

func TestRefreshLockUsesContextDeadline(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	_, unlock, err := openStore("shared", true)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	_, err = refreshStoredSession(ctx, "shared", session{}, true, io.Discard)
	var ae *apiError
	if ctx.Err() != context.DeadlineExceeded || !errors.As(err, &ae) || ae.Code != "busy" {
		t.Fatalf("expected busy when the context budget elapsed, got %v", err)
	}
}

func TestDoctorRefreshesExpiredSession(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	transport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = transport })
	exchanges := 0
	http.DefaultTransport = testTransport(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		switch r.URL.Path {
		case "/oauth/token":
			exchanges++
			json.NewEncoder(w).Encode(tokenResponse{AccessToken: "new-access", RefreshToken: "new-refresh", TokenType: "Bearer", Scope: agentScope, ExpiresIn: 900})
		case "/api/v1/me":
			if r.Header.Get("Authorization") != "Bearer new-access" {
				t.Fatal("doctor did not use refreshed session")
			}
			json.NewEncoder(w).Encode(identity{TenantNumber: "12345678", Scope: agentScope})
		default:
			t.Fatal("unexpected request")
		}
		return w.Result(), nil
	})
	saveTestSession(t, "work", session{Server: "https://example.invalid", TenantNumber: "12345678", Scope: agentScope,
		AccessToken: "expired", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(-time.Hour)})
	for _, status := range []string{"refreshed", "active"} {
		var out bytes.Buffer
		if code := Run(t.Context(), []string{"--profile", "work", "doctor", "--json"}, &out, io.Discard); code != 0 || !strings.Contains(out.String(), `"login":"`+status+`"`) || exchanges != 1 {
			t.Fatalf("unexpected doctor status (%d): %s", code, &out)
		}
	}
}

func saveIdentityTestProfile(t *testing.T, server, scope string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	saveTestSession(t, "work", session{Server: server, TenantNumber: "12345678", Scope: scope,
		AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", ExpiresAt: time.Now().Add(time.Hour)})
}

func TestPreparationChecksIdentityBeforeReturningConfirmation(t *testing.T) {
	const tenant = "12345678"
	for _, tc := range []struct {
		name, tenant, scope, storedScope, wantError string
		legacy                                      bool
	}{
		{name: "one request", tenant: tenant, scope: agentScope, storedScope: agentScope},
		{name: "remember scope from headers", tenant: tenant, scope: agentScope},
		{name: "remember legacy scope", tenant: tenant, scope: legacyReadScope, legacy: true},
		{name: "changed tenant", tenant: "87654321", scope: agentScope, storedScope: agentScope, wantError: "Connection tenant changed. Refusing the request."},
		{name: "changed scope", tenant: tenant, scope: "isms:read", storedScope: agentScope, wantError: "Connection scope changed. Refusing the request."},
		{name: "missing scope header", tenant: tenant, storedScope: agentScope, wantError: "incomplete or ambiguous"},
		{name: "unsupported scope", tenant: tenant, scope: "unknown", wantError: "expected access"},
		{name: "changed legacy tenant", tenant: "87654321", scope: agentScope, storedScope: agentScope, legacy: true, wantError: "Connection tenant changed. Refusing the request."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var preparations, lookups int
			server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case apiPaths["api/commands#create"]:
					preparations++
					if !tc.legacy {
						w.Header().Set("Valiido-Tenant", tc.tenant)
						if tc.scope != "" {
							w.Header().Set("Valiido-Scope", tc.scope)
						}
					}
					w.WriteHeader(http.StatusAccepted)
					writeCommandResult(w, map[string]any{"pending_confirmation": true, "confirmation_token": "synthetic-confirmation"})
				case apiPaths["api/identities#show"]:
					lookups++
					json.NewEncoder(w).Encode(identity{TenantNumber: tc.tenant, Scope: tc.scope})
				default:
					t.Error("unexpected request", r.URL.Path)
				}
			})
			saveIdentityTestProfile(t, server.URL, tc.storedScope)
			var out bytes.Buffer
			code := Run(t.Context(), []string{"tasks", "update", "--profile", "work", "--json"}, &out, io.Discard)
			if preparations != 1 || tc.legacy && lookups != 1 || !tc.legacy && lookups != 0 {
				t.Fatalf("preparations=%d identity lookups=%d", preparations, lookups)
			}
			if tc.wantError != "" {
				if code == 0 || !strings.Contains(out.String(), tc.wantError) || strings.Contains(out.String(), "synthetic-confirmation") {
					t.Fatalf("exit=%d output=%s", code, &out)
				}
			} else if code != 0 || !strings.Contains(out.String(), "synthetic-confirmation") {
				t.Fatalf("exit=%d output=%s", code, &out)
			}
			store, err := profileStore("work", false)
			if err != nil {
				t.Fatal(err)
			}
			value, err := store.load()
			wantScope := tc.storedScope
			if tc.wantError == "" {
				wantScope = tc.scope
			}
			if err != nil || value.Scope != wantScope {
				t.Fatalf("stored scope=%q want=%q err=%v", value.Scope, wantScope, err)
			}
		})
	}
}

func TestLegacyLookupIsSharedAcrossPagesAndLaterHeadersAreChecked(t *testing.T) {
	for _, tc := range []struct {
		name    string
		changed bool
	}{
		{"legacy pages", false},
		{"changed second page", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var pages, lookups int
			server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case apiPaths["api/identities#show"]:
					lookups++
					json.NewEncoder(w).Encode(identity{TenantNumber: "12345678", Scope: agentScope})
				case apiPaths["api/commands#create"]:
					pages++
					page := resourcePage{Type: "tasks", Resources: []resource{}, Complete: pages == 2}
					if pages == 1 {
						page.NextCursor = "second"
					} else if tc.changed {
						w.Header().Set("Valiido-Tenant", "87654321")
						w.Header().Set("Valiido-Scope", agentScope)
					}
					writeCommandResult(w, page)
				default:
					t.Error("unexpected request", r.URL.Path)
				}
			})
			saveIdentityTestProfile(t, server.URL, agentScope)
			var out bytes.Buffer
			code := Run(t.Context(), []string{"tasks", "list", "--profile", "work", "--json"}, &out, io.Discard)
			if pages != 2 || lookups != 1 {
				t.Fatalf("pages=%d identity lookups=%d", pages, lookups)
			}
			if tc.changed {
				if code == 0 || !strings.Contains(out.String(), "Connection tenant changed") {
					t.Fatalf("exit=%d output=%s", code, &out)
				}
			} else if code != 0 {
				t.Fatalf("exit=%d output=%s", code, &out)
			}
		})
	}
}

type unreadBody struct {
	t      *testing.T
	closed bool
}

func (b *unreadBody) Read([]byte) (int, error) {
	b.t.Error("read response body before verifying connection identity")
	return 0, io.EOF
}
func (b *unreadBody) Close() error { b.closed = true; return nil }

func TestMismatchedResponseIsClosedBeforeBufferingOrRetry(t *testing.T) {
	for _, status := range []int{200, 401, 429} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			c, err := newClient("https://example.invalid", "synthetic-access")
			if err != nil {
				t.Fatal(err)
			}
			c.useSession("work", session{TenantNumber: "12345678", Scope: agentScope, ExpiresAt: time.Now().Add(time.Hour)})
			body := &unreadBody{t: t}
			requests := 0
			c.http.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
				requests++
				return &http.Response{StatusCode: status, Body: body, Header: http.Header{
					"Valiido-Tenant": {"87654321"}, "Valiido-Scope": {agentScope},
				}}, nil
			})
			_, err = c.runCommand(t.Context(), "tasks.list", "{}", false)
			if err == nil || err.Error() != "Connection tenant changed. Refusing the request." || requests != 1 || !body.closed {
				t.Fatalf("err=%v requests=%d closed=%t", err, requests, body.closed)
			}
		})
	}
}

func TestLoginKeepsTheProfileServer(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	saveTestSession(t, "local", session{Server: "http://127.0.0.1:3000", TenantNumber: "12345678", Scope: agentScope})
	for profile, want := range map[string]string{"local": "http://127.0.0.1:3000", "fresh": "https://app.valiido.com"} {
		var out bytes.Buffer
		if code := Run(t.Context(), []string{"auth", "login", "--profile", profile, "--dry-run", "--json"}, &out, io.Discard); code != 0 ||
			!strings.Contains(out.String(), `"server":"`+want+`"`) {
			t.Errorf("%s: login targets %s, want %s", profile, &out, want)
		}
	}
}

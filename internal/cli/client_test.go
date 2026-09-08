package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOriginValidation(t *testing.T) {
	for _, server := range []string{"http://example.com", "https://user:secret@example.com", "https://example.com/path", "https://example.com?x=y", "https://example.com#fragment", "file:///tmp/token"} {
		if _, err := newClient(server, "secret"); err == nil {
			t.Errorf("accepted unsafe origin %q", server)
		}
	}
	for _, server := range []string{"https://app.valiido.com", "http://127.0.0.1:3000", "http://[::1]:3000"} {
		if _, err := newClient(server, "secret"); err != nil {
			t.Errorf("rejected valid origin %q: %v", server, err)
		}
	}
}

func TestCompletePaginationAndBearer(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "Bearer secret" || r.URL.Path != "/api/v1/commands" {
			t.Error("missing authentication or wrong command path")
		}
		r.ParseForm()
		if r.Form.Get("name") != "documents.list" {
			t.Error("wrong command")
		}
		if requests == 1 {
			writeCommandResult(w, resourcePage{Type: "documents", Resources: []resource{documentResource(1, "rich_text")}, NextCursor: "opaque&?+"})
		} else {
			var input map[string]any
			json.Unmarshal([]byte(r.Form.Get("input")), &input)
			if input["cursor"] != "opaque&?+" {
				t.Error("cursor was not encoded safely")
			}
			writeCommandResult(w, resourcePage{Type: "documents", Resources: []resource{documentResource(2, "folder")}, Complete: true})
		}
	}))
	defer server.Close()
	c, _ := newClient(server.URL, "secret")
	docs, err := c.documents(context.Background())
	if err != nil || len(docs) != 2 || requests != 2 {
		t.Fatalf("did not collect every page: %v %v", docs, err)
	}
}

func TestPaginationFailureNeverReturnsPartialSuccess(t *testing.T) {
	for _, mode := range []string{"failure", "loop", "duplicate"} {
		t.Run(mode, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if requests > 1 && mode == "failure" {
					http.Error(w, "secret server exception", 500)
					return
				}
				id := int64(requests)
				if mode == "duplicate" {
					id = 1
				}
				writeCommandResult(w, resourcePage{Type: "documents", Resources: []resource{documentResource(id, "rich_text")}, NextCursor: "same"})
			}))
			defer server.Close()
			c, _ := newClient(server.URL, "secret")
			docs, err := c.documents(context.Background())
			if err == nil || docs != nil || strings.Contains(err.Error(), "secret server") {
				t.Fatalf("partial success or information leak: %v %v", docs, err)
			}
		})
	}
}

func TestRedirectDoesNotLeakCredential(t *testing.T) {
	called := false
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer other.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, 302) }))
	defer server.Close()
	c, _ := newClient(server.URL, "secret")
	_, err := c.me(context.Background())
	if err == nil || called {
		t.Fatal("followed credential-bearing redirect")
	}
}

func TestPairingRejectsForeignOriginAndCancels(t *testing.T) {
	for _, uri := range []string{"https://other.example/verify", "same"} {
		t.Run(uri, func(t *testing.T) {
			var origin string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				verification := uri
				if verification == "same" {
					verification = origin + "/api/pairing"
				}
				json.NewEncoder(w).Encode(deviceResponse{DeviceCode: "device-secret", UserCode: "ABCD-1234", VerificationURI: verification, ExpiresIn: 600, Interval: 5})
			}))
			defer server.Close()
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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.Form.Get("agent_name") == "" {
			t.Fatal("device authorization omitted machine name")
		}
		json.NewEncoder(w).Encode(deviceResponse{
			DeviceCode: "device-secret", UserCode: "ABCD-1234",
			VerificationURI:         origin + "/settings?section=agents",
			VerificationURIComplete: origin + "/settings?section=agents&user_code=ABCD-1234&agent_name=workstation-7",
			ExpiresIn:               600, Interval: 5,
		})
	}))
	defer server.Close()
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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(deviceResponse{
			DeviceCode: "device-secret", UserCode: "ABCD-1234",
			VerificationURI:         "http://" + r.Host + "/settings?section=agents",
			VerificationURIComplete: "https://other.example/settings?user_code=ABCD-1234",
			ExpiresIn:               600, Interval: 5,
		})
	}))
	defer server.Close()
	c, _ := newClient(server.URL, "")
	var instructions bytes.Buffer
	if _, err := c.pair(context.Background(), &instructions); err == nil || instructions.Len() != 0 {
		t.Fatal("accepted a foreign complete pairing link")
	}
}

func TestFileCredentialsAndProfileLock(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	s, unlock, err := openStore("test", true)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, _, err := openStore("test", true); err == nil {
		t.Fatal("concurrent process acquired same profile")
	}
	value := session{Server: "https://app.valiido.com", TenantNumber: "12345678", AccessToken: "secret", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour)}
	if err := s.save(value); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(s.path())
	if info.Mode().Perm() != 0600 {
		t.Fatalf("unsafe file permissions: %v", info.Mode())
	}
	got, err := s.load()
	if err != nil || got.AccessToken != "secret" {
		t.Fatal("could not load saved connection")
	}
	if err := os.Chmod(s.path(), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.load(); err == nil {
		t.Fatal("accepted world-readable credentials")
	}
}

func TestCLIRefreshPreservesLegacyReadScopeAndLogout(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	refreshes, revokes := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			writeCommandResult(w, resource{Type: "documents", ID: 42, Label: "Test", Attributes: map[string]any{
				"title": "Test", "kind": "rich_text", "content_state": "text", "content_text": "Hello\x1b[31m\nWorld",
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
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

func TestPDFDownloadDoesNotOverwriteOrSaveErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		io.WriteString(w, "%PDF-1.4\nTest\n%%EOF")
	}))
	defer server.Close()
	c, _ := newClient(server.URL, "secret")
	path := filepath.Join(t.TempDir(), "test.pdf")
	if err := c.download(context.Background(), "risks", 1, path); err != nil {
		t.Fatal(err)
	}
	if err := c.download(context.Background(), "risks", 1, path); err == nil {
		t.Fatal("overwrote existing PDF")
	}
}

func documentResource(id int64, kind string) resource {
	return resource{Type: "documents", ID: id, Label: "Document", Attributes: map[string]any{"title": "Document", "kind": kind}}
}

func writeCommandResult(w http.ResponseWriter, data any) {
	encoded, _ := json.Marshal(data)
	var object map[string]any
	json.Unmarshal(encoded, &object)
	json.NewEncoder(w).Encode(commandResult{OK: true, Data: object, Summary: "Done"})
}

func TestTokenScopeAndHTTPErrorRedaction(t *testing.T) {
	if validateToken(tokenResponse{AccessToken: "x", RefreshToken: "y", TokenType: "Bearer", Scope: "admin", ExpiresIn: 900}, "") == nil {
		t.Fatal("accepted admin scope")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		io.WriteString(w, `{"error":"invalid_grant","error_description":"secret-token"}`)
	}))
	defer server.Close()
	c, _ := newClient(server.URL, "")
	err := c.json(context.Background(), "POST", "/oauth/token", url.Values{}, false, true, nil)
	if err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatal("leaked server error")
	}
}

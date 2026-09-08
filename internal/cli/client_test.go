package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

func TestResourceQueryTravelsAcrossPages(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		var input map[string]any
		if json.Unmarshal([]byte(r.Form.Get("input")), &input) != nil || input["query"] != "Customer & cloud" {
			t.Fatalf("query missing from page %d: %#v", requests, input)
		}
		page := resourcePage{Type: "assets", Resources: []resource{}, Complete: requests == 2}
		if requests == 1 {
			page.NextCursor = "next"
		}
		writeCommandResult(w, page)
	}))
	defer server.Close()
	c, _ := newClient(server.URL, "secret")
	if _, err := c.resources(context.Background(), "assets", "Customer & cloud"); err != nil || requests != 2 {
		t.Fatalf("filtered pagination failed: requests=%d err=%v", requests, err)
	}
}

func TestExamplesCollectEveryPageWithStableQuery(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if err := r.ParseForm(); err != nil || r.Form.Get("name") != "assets.list_examples" {
			t.Fatal("wrong example command")
		}
		var input map[string]any
		if json.Unmarshal([]byte(r.Form.Get("input")), &input) != nil || input["query"] != "cloud" || input["limit"] != float64(2) {
			t.Fatalf("query or page size changed: %#v", input)
		}
		if requests == 1 {
			if input["cursor"] != "" {
				t.Fatal("first page unexpectedly had a cursor")
			}
			writeCommandResult(w, examplePage{Type: "assets", Examples: []example{{ExampleID: 1, Label: "One", Attributes: map[string]any{"asset_name": "One"}}}, NextCursor: "signed&opaque"})
			return
		}
		if input["cursor"] != "signed&opaque" {
			t.Fatalf("cursor was not preserved: %#v", input)
		}
		writeCommandResult(w, examplePage{Type: "assets", Examples: []example{{ExampleID: 2, Label: "Two", Attributes: map[string]any{"asset_name": "Two"}}}, Complete: true})
	}))
	defer server.Close()
	c, _ := newClient(server.URL, "secret")
	page, err := c.examples(context.Background(), "assets", "cloud", 2)
	if err != nil || requests != 2 || len(page.Examples) != 2 || !page.Complete || page.NextCursor != "" {
		t.Fatalf("examples were not fully collected: requests=%d page=%#v err=%v", requests, page, err)
	}
}

func TestExamplesRejectBrokenPagination(t *testing.T) {
	for _, mode := range []string{"duplicate", "loop", "inconsistent"} {
		t.Run(mode, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				page := examplePage{Type: "assets", Examples: []example{{ExampleID: 1, Label: "One", Attributes: map[string]any{}}}, NextCursor: "same"}
				if mode == "inconsistent" {
					page.Complete = true
				}
				writeCommandResult(w, page)
			}))
			defer server.Close()
			c, _ := newClient(server.URL, "secret")
			if _, err := c.examples(context.Background(), "assets", "", 50); err == nil {
				t.Fatal("accepted unsafe example pagination")
			}
		})
	}
}

func TestCommandCatalogFiltersAreEncoded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/commands" || r.URL.Query().Get("query") != "asset create" || r.URL.Query().Get("limit") != "7" {
			t.Fatalf("unexpected catalog request: %s", r.URL.String())
		}
		json.NewEncoder(w).Encode(commandCatalog{Commands: []map[string]any{}})
	}))
	defer server.Close()
	c, _ := newClient(server.URL, "secret")
	if _, err := c.commands(context.Background(), "asset create", 7); err != nil {
		t.Fatal(err)
	}
}

func TestSearchOptionLimitsFailBeforeReadingCredentials(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, args := range [][]string{
		{"command", "list", "--limit", "0"},
		{"command", "list", "--limit", "251"},
		{"resource", "list", "assets", "--query", strings.Repeat("x", 1025)},
	} {
		var out bytes.Buffer
		if code := Run(context.Background(), args, &out, io.Discard); code != 2 {
			t.Fatalf("expected usage error for %v, got %d: %s", args, code, out.String())
		}
	}
}

func TestInvalidCLIInputFailsBeforeCredentialsOrNetwork(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	invalidImport := filepath.Join(t.TempDir(), "broken.json")
	if err := os.WriteFile(invalidImport, []byte(`[{`), 0600); err != nil {
		t.Fatal(err)
	}
	oversizedCommandInput := filepath.Join(t.TempDir(), "oversized-command.json")
	if err := os.WriteFile(oversizedCommandInput, []byte(strings.Repeat("x", maxCommandInput+1)), 0600); err != nil {
		t.Fatal(err)
	}
	cases := [][]string{
		{"auth"},
		{"resource", "list"},
		{"resource", "read", "assets"},
		{"resource", "read", "assets", "0"},
		{"resource", "examples"},
		{"resource", "import", "assets"},
		{"resource", "import", "assets", "--file", invalidImport},
		{"resource", "pdf", "assets", "1"},
		{"command", "run"},
		{"command", "run", "assets.create", "--input", "{"},
		{"command", "run", "assets.create", "--input", "{}", "--input-file", invalidImport},
		{"command", "run", "assets.create", "--input-file", invalidImport},
		{"command", "run", "assets.create", "--input-file", oversizedCommandInput},
		{"command", "confirm"},
		{"command", "confirm", "has whitespace"},
		{"command", "list", "--profile", "one", "--profile", "two"},
		{"command", "list", "--query=one", "--query=two"},
		{"resource", "list", strings.Repeat("a", 65)},
		{"command", "run", strings.Repeat("a", 65) + ".create"},
		{"command", "confirm", strings.Repeat("x", 4097)},
		{"auth", "status", "--profile"},
		{"resource", "list", "assets", "--query"},
		{"resource", "examples", "assets", "--limit"},
		{"resource", "import", "assets", "--file"},
		{"resource", "pdf", "assets", "1", "--output"},
		{"command", "run", "assets.create", "--input"},
		{"command", "run", "assets.create", "--dry-run", "--dry-run"},
		{"command", "list", "--json", "--json"},
		{"command", "list", "--profile", strings.Repeat("p", 65)},
		{"resource", "list", "assets", "--query", strings.Repeat("q", 1025)},
		{"command", "run", "assets.create", "--input", strings.Repeat("x", maxCommandInput+1)},
	}
	for _, args := range cases {
		var out bytes.Buffer
		if code := Run(context.Background(), args, &out, io.Discard); code != 2 {
			t.Errorf("%v returned %d instead of usage: %s", args, code, out.String())
		}
	}
}

func TestHTTPStatusMapping(t *testing.T) {
	// The 429 case exercises the rate-limit retry loop; run it without real waits.
	defer func(base time.Duration) { retryBackoffBase = base }(retryBackoffBase)
	retryBackoffBase = 0

	tests := []struct {
		status int
		body   string
		code   string
		exit   int
	}{
		{http.StatusUnauthorized, `{}`, "auth", 3},
		{http.StatusForbidden, `{}`, "forbidden", 4},
		{http.StatusNotFound, `{}`, "not_found", 5},
		{http.StatusConflict, `{"error":{"code":"conflict","message":"The resource changed."}}`, "conflict", 9},
		{http.StatusConflict, `{}`, "conflict", 9},
		{http.StatusUnprocessableEntity, `{"error":{"code":"invalid_input","message":"Input is invalid."}}`, "invalid_input", 6},
		{http.StatusUnprocessableEntity, `{}`, "invalid_input", 6},
		{http.StatusTooManyRequests, `{}`, "rate_limited", 7},
	}
	for _, test := range tests {
		t.Run(strconv.Itoa(test.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.status)
				io.WriteString(w, test.body)
			}))
			defer server.Close()
			c, _ := newClient(server.URL, "token")
			err := c.json(context.Background(), "GET", "/test", nil, true, false, nil)
			var apiErr *apiError
			if !errors.As(err, &apiErr) || apiErr.Code != test.code || exitCode(apiErr.Code) != test.exit {
				t.Fatalf("status %d mapped to %#v", test.status, err)
			}
		})
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

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestPairingStopsWhenInstructionsCannotBeWritten(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		json.NewEncoder(w).Encode(deviceResponse{
			DeviceCode: "device-code", UserCode: "ABCD-1234",
			VerificationURI: "http://" + r.Host + "/settings", ExpiresIn: 60, Interval: 1,
		})
	}))
	defer server.Close()
	c, _ := newClient(server.URL, "")
	if _, err := c.pair(context.Background(), failingWriter{}); err == nil || requests != 1 {
		t.Fatalf("pairing continued after output failure: requests=%d err=%v", requests, err)
	}
}

func FuzzParseResourceID(f *testing.F) {
	for _, seed := range []string{"", "0", "1", "-1", "42", "9223372036854775807", "9223372036854775808", "1.0", "１２"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		id, err := parseResourceID(raw)
		if err == nil && id <= 0 {
			t.Fatalf("accepted non-positive ID %q", raw)
		}
	})
}

func FuzzResponseErrorIsAlwaysTyped(f *testing.F) {
	f.Add(422, []byte(`{"error":{"code":"invalid_input","message":"Invalid."}}`))
	f.Add(500, []byte("broken"))
	f.Fuzz(func(t *testing.T, status int, body []byte) {
		var apiErr *apiError
		if !errors.As(responseError(status, body), &apiErr) || apiErr.Code == "" || apiErr.Message == "" {
			t.Fatal("response error was not safe and typed")
		}
	})
}

func TestFileCredentialsAndProfileLock(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	s, unlock, err := openStore("test", true)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(2 * profileLockPoll)
		unlock()
	}()
	_, secondUnlock, err := openStore("test", true)
	if err != nil {
		t.Fatalf("concurrent process did not wait for the profile: %v", err)
	}
	secondUnlock()
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

func TestStaleProfileLockIsReclaimed(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "valiido")
	if err := os.MkdirAll(filepath.Join(dir, "test.lock"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeLockOwner(filepath.Join(dir, "test.lock"), lockOwner{PID: 2147483647, Nonce: "dead"}); err != nil {
		t.Fatal(err)
	}
	s, unlock, err := openStore("test", true)
	if err != nil || s == nil {
		t.Fatalf("stale lock was not reclaimed: %v", err)
	}
	unlock()
}

func TestOldOwnerlessProfileLockIsReclaimed(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	lock := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "valiido", "legacy.lock")
	if err := os.MkdirAll(lock, 0700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * orphanLockGrace)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	_, unlock, err := openStore("legacy", true)
	if err != nil {
		t.Fatalf("ownerless crash lock was not reclaimed: %v", err)
	}
	unlock()
}

func TestConcurrentCommandsRotateRefreshTokenOnceAndThenRunInParallel(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var refreshes atomic.Int32
	var active atomic.Int32
	var maximum atomic.Int32
	commandsReady := make(chan struct{}, 2)
	releaseCommands := make(chan struct{})
	var releaseOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			refreshes.Add(1)
			json.NewEncoder(w).Encode(tokenResponse{AccessToken: "new-access", RefreshToken: "new-refresh", TokenType: "Bearer", Scope: agentScope, ExpiresIn: 900})
		case "/api/v1/me":
			json.NewEncoder(w).Encode(identity{TenantNumber: "12345678", Scope: agentScope, AgentName: "Agent"})
		case "/api/v1/commands":
			current := active.Add(1)
			defer active.Add(-1)
			for {
				old := maximum.Load()
				if current <= old || maximum.CompareAndSwap(old, current) {
					break
				}
			}
			commandsReady <- struct{}{}
			select {
			case <-releaseCommands:
			case <-time.After(time.Second):
			}
			writeCommandResult(w, resourcePage{Type: "tasks", Resources: []resource{}, Complete: true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	s, unlock, err := openStore("shared", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.save(session{Server: server.URL, TenantNumber: "12345678", Scope: agentScope, AccessToken: "old", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	unlock()

	results := make(chan int, 2)
	for range 2 {
		go func() {
			var out bytes.Buffer
			results <- Run(context.Background(), []string{"task", "list", "--profile", "shared"}, &out, io.Discard)
		}()
	}
	for range 2 {
		select {
		case <-commandsReady:
		case <-time.After(2 * time.Second):
			t.Fatal("commands did not run concurrently")
		}
	}
	releaseOnce.Do(func() { close(releaseCommands) })
	for range 2 {
		if code := <-results; code != 0 {
			t.Fatalf("concurrent command exited %d", code)
		}
	}
	if refreshes.Load() != 1 || maximum.Load() != 2 {
		t.Fatalf("refreshes=%d maximum concurrency=%d", refreshes.Load(), maximum.Load())
	}
}

func TestCLIProcessHelper(t *testing.T) {
	if os.Getenv("VALIIDO_CLI_PROCESS_HELPER") != "1" {
		return
	}
	os.Exit(Run(context.Background(), []string{"task", "list", "--profile", "shared"}, io.Discard, io.Discard))
}

func TestSeparateProcessesShareAProfileWithoutRefreshRace(t *testing.T) {
	config := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", config)
	var refreshes atomic.Int32
	var active atomic.Int32
	var maximum atomic.Int32
	commandsReady := make(chan struct{}, 2)
	releaseCommands := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			refreshes.Add(1)
			json.NewEncoder(w).Encode(tokenResponse{AccessToken: "new-access", RefreshToken: "new-refresh", TokenType: "Bearer", Scope: agentScope, ExpiresIn: 900})
		case "/api/v1/me":
			json.NewEncoder(w).Encode(identity{TenantNumber: "12345678", Scope: agentScope, AgentName: "Agent"})
		case "/api/v1/commands":
			current := active.Add(1)
			defer active.Add(-1)
			for {
				old := maximum.Load()
				if current <= old || maximum.CompareAndSwap(old, current) {
					break
				}
			}
			commandsReady <- struct{}{}
			select {
			case <-releaseCommands:
			case <-time.After(2 * time.Second):
			}
			writeCommandResult(w, resourcePage{Type: "tasks", Resources: []resource{}, Complete: true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	s, unlock, err := openStore("shared", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.save(session{Server: server.URL, TenantNumber: "12345678", Scope: agentScope, AccessToken: "old", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	unlock()

	commands := make([]*exec.Cmd, 2)
	for index := range commands {
		commands[index] = exec.Command(os.Args[0], "-test.run=^TestCLIProcessHelper$")
		commands[index].Env = append(os.Environ(), "VALIIDO_CLI_PROCESS_HELPER=1", "XDG_CONFIG_HOME="+config)
		if err := commands[index].Start(); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		select {
		case <-commandsReady:
		case <-time.After(4 * time.Second):
			close(releaseCommands)
			t.Fatal("separate CLI processes did not reach commands concurrently")
		}
	}
	close(releaseCommands)
	for _, command := range commands {
		if err := command.Wait(); err != nil {
			t.Fatalf("CLI subprocess failed: %v", err)
		}
	}
	if refreshes.Load() != 1 || maximum.Load() != 2 {
		t.Fatalf("refreshes=%d maximum process concurrency=%d", refreshes.Load(), maximum.Load())
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
			writeCommandResult(w, resource{Type: "documents", ID: 42, Version: resourceVersion(2), Label: "Test", Attributes: map[string]any{
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

func TestPDFDownloadPreservesJSONAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{"error": apiError{Code: "not_found", Message: "The requested asset could not be found."}})
	}))
	defer server.Close()
	c, _ := newClient(server.URL, "secret")
	output := filepath.Join(t.TempDir(), "missing.pdf")
	err := c.download(context.Background(), "assets", 99, output)
	var apiErr *apiError
	if !errors.As(err, &apiErr) || apiErr.Code != "not_found" || exitCode(apiErr.Code) != 5 {
		t.Fatalf("PDF API error was lost: %#v", err)
	}
	if _, statErr := os.Stat(output); !os.IsNotExist(statErr) {
		t.Fatal("saved an API error as a PDF")
	}
}

func TestAbortedPDFDownloadRemovesPartialFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("Content-Length", "100")
		io.WriteString(w, "%PDF-short")
	}))
	defer server.Close()
	c, _ := newClient(server.URL, "token")
	output := filepath.Join(t.TempDir(), "partial.pdf")
	if err := c.download(context.Background(), "assets", 1, output); err == nil {
		t.Fatal("accepted an incomplete PDF download")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("partial PDF remained on disk")
	}
}

func TestExitCodeMappingAndOversizedResourceID(t *testing.T) {
	if exitCode("not_found") != 5 || exitCode("invalid_input") != 6 || exitCode("not_authorized") != 4 || exitCode("storage") != 8 {
		t.Fatal("unexpected API exit code mapping")
	}
	var out bytes.Buffer
	if code := Run(context.Background(), []string{"resource", "read", "assets", "9223372036854775808"}, &out, io.Discard); code != 2 || !strings.Contains(out.String(), "9223372036854775807") {
		t.Fatalf("oversized ID was not a clear usage error: code=%d output=%s", code, out.String())
	}
}

func TestContextualHelp(t *testing.T) {
	for _, args := range [][]string{{"resource", "--help"}, {"command", "run", "--help"}, {"resource", "import", "-h"}} {
		var out bytes.Buffer
		if code := Run(context.Background(), args, &out, io.Discard); code != 0 || !strings.HasPrefix(out.String(), "Usage:\n") {
			t.Fatalf("%v did not render contextual help: code=%d output=%q", args, code, out.String())
		}
	}
}

func TestRateLimitRetryPreservesPostAndAuthorization(t *testing.T) {
	defer func(base time.Duration) { retryBackoffBase = base }(retryBackoffBase)
	retryBackoffBase = 0
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer secret" {
			t.Fatalf("retry changed method or authorization")
		}
		if err := r.ParseForm(); err != nil || r.Form.Get("name") != "tasks.list" || r.Form.Get("input") != `{}` {
			t.Fatalf("retry changed form: %#v (%v)", r.Form, err)
		}
		w.Header().Set("Content-Type", "application/json")
		if requests < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{}`)
			return
		}
		io.WriteString(w, `{"ok":true,"data":{},"summary":"Done"}`)
	}))
	defer server.Close()
	c, _ := newClient(server.URL, "secret")
	var result commandResult
	if err := c.json(context.Background(), http.MethodPost, "/api/v1/commands", url.Values{"name": {"tasks.list"}, "input": {"{}"}}, true, false, &result); err != nil || requests != 3 {
		t.Fatalf("retry failed: requests=%d err=%v", requests, err)
	}
}

func TestRateLimitRetryIsBoundedAndCancellable(t *testing.T) {
	defer func(base time.Duration) { retryBackoffBase = base }(retryBackoffBase)
	retryBackoffBase = 0
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	c, _ := newClient(server.URL, "secret")
	resp, err := c.request(context.Background(), http.MethodGet, "/limited", nil, true)
	if err != nil || resp == nil || requests != maxRateLimitRetries+1 {
		t.Fatalf("retry bound failed: requests=%d response=%v err=%v", requests, resp, err)
	}
	resp.Body.Close()

	retryBackoffBase = time.Second
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.request(ctx, http.MethodGet, "/limited", nil, true); err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("cancelled retry returned %v", err)
	}
}

func documentResource(id int64, kind string) resource {
	return resource{Type: "documents", ID: id, Version: resourceVersion(3), Label: "Document", Attributes: map[string]any{"title": "Document", "kind": kind}}
}

func resourceVersion(value int64) *int64 { return &value }

func TestResourceVersionIsRequiredAndNonNegative(t *testing.T) {
	for _, item := range []resource{
		{Type: "tasks", ID: 1, Attributes: map[string]any{}},
		{Type: "tasks", ID: 1, Version: resourceVersion(-1), Attributes: map[string]any{}},
	} {
		if err := validateResource(item, "tasks"); err == nil {
			t.Fatalf("accepted invalid resource version: %#v", item.Version)
		}
	}

	if err := validateResource(resource{Type: "tasks", ID: 1, Version: resourceVersion(0), Attributes: map[string]any{}}, "tasks"); err != nil {
		t.Fatalf("rejected initial resource version: %v", err)
	}
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

func TestListResponsesStayWithinClientSafetyBounds(t *testing.T) {
	resources := make([]resource, pageSize+1)
	for index := range resources {
		resources[index] = resource{Type: "assets", ID: int64(index + 1), Version: resourceVersion(0), Attributes: map[string]any{}}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeCommandResult(w, resourcePage{Type: "assets", Resources: resources, Complete: true})
	}))
	defer server.Close()
	c, _ := newClient(server.URL, "secret")
	if _, err := c.resources(context.Background(), "assets", ""); err == nil {
		t.Fatal("accepted a resource page larger than requested")
	}

	if _, err := c.examples(context.Background(), "assets", "", pageSize+1); err == nil {
		t.Fatal("accepted an unsafe example page size")
	}
}

func TestResourceListHasAnAggregateSafetyLimit(t *testing.T) {
	request := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		items := make([]resource, pageSize)
		for index := range items {
			items[index] = resource{
				Type:       "assets",
				ID:         int64(request*pageSize + index + 1),
				Version:    resourceVersion(0),
				Attributes: map[string]any{},
			}
		}
		request++
		writeCommandResult(w, resourcePage{Type: "assets", Resources: items, NextCursor: strconv.Itoa(request)})
	}))
	defer server.Close()
	c, _ := newClient(server.URL, "secret")
	if _, err := c.resources(context.Background(), "assets", ""); err == nil || request != maxListItems/pageSize+1 {
		t.Fatalf("aggregate safety limit was not enforced: requests=%d err=%v", request, err)
	}
}

func TestCommandResultEnvelopeIsValidated(t *testing.T) {
	for _, result := range []commandResult{
		{Summary: "Looks successful"},
		{OK: true, Summary: "Success", Error: &apiError{Code: "invalid_input", Message: "contradictory"}},
		{},
	} {
		if err := validateCommandResult(result); err == nil {
			t.Fatalf("accepted contradictory command result: %#v", result)
		}
	}
	want := &apiError{Code: "conflict", Message: "Changed"}
	err := validateCommandResult(commandResult{Error: want})
	var got *apiError
	if !errors.As(err, &got) || got.Code != want.Code {
		t.Fatalf("lost structured command error: %#v", err)
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

func TestOversizedCredentialFileIsRejectedWithoutReadingItAll(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	s, unlock, err := openStore("oversized", true)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := os.WriteFile(s.path(), bytes.Repeat([]byte("x"), maxStoredCredentialBytes+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.load(); err == nil {
		t.Fatal("accepted oversized stored credentials")
	}
}

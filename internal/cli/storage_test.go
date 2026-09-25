package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

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

func TestProfileLockOwnerAndCancellation(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	s, unlock, err := openStore("held", true)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	path := filepath.Join(s.dir, "held.lock")
	owner, err := readLockOwner(path)
	if err != nil || owner.PID != os.Getpid() || owner.StartedAt.IsZero() {
		t.Fatalf("invalid lock owner: %#v %v", owner, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	_, _, err = openStoreContext(ctx, "held", true, time.Second)
	var ae *apiError
	if !errors.As(err, &ae) || ae.Code != "cancelled" {
		t.Fatalf("lock wait did not stop on cancellation: %v", err)
	}
	unlock()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock still exists after release: %v", err)
	}
}

func TestDoctorDoesNotWaitForBusyProfile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	_, unlock, err := openStore("check", true)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	var out bytes.Buffer
	if code := Run(t.Context(), []string{"doctor", "--profile", "check", "--json"}, &out, io.Discard); code != 7 {
		t.Fatalf("doctor exit=%d: %s", code, out.String())
	}
	if !strings.Contains(out.String(), `"lock":"busy"`) || !strings.Contains(out.String(), `"version":`) {
		t.Fatalf("doctor missed lock/version status: %s", out.String())
	}
}

func TestConcurrentCommandsRotateRefreshTokenOnceAndThenRunInParallel(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var refreshes atomic.Int32
	var active atomic.Int32
	var maximum atomic.Int32
	commandsReady := make(chan struct{}, 2)
	releaseCommands := make(chan struct{})
	var releaseOnce sync.Once
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
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
	})

	saveTestSession(t, "shared", session{Server: server.URL, TenantNumber: "12345678", Scope: agentScope, AccessToken: "old", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(-time.Hour)})

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
	// Parent and child exchange HTTP messages through their private pipes.
	input := bufio.NewReader(os.Stdin)
	http.DefaultTransport = testTransport(func(r *http.Request) (*http.Response, error) {
		if err := r.Write(os.Stderr); err != nil {
			return nil, err
		}
		return http.ReadResponse(input, r)
	})
	os.Exit(Run(context.Background(), []string{"tasks", "list", "--limit", "1", "--profile", "shared", "--json"}, os.Stdout, io.Discard))
}

const sharedProfileProcesses = 60

type sharedProfileServer struct {
	mu                                                         sync.Mutex
	access, refresh                                            string
	expires                                                    time.Time
	issued                                                     map[string]bool
	revoked, rateLimitNext                                     bool
	rotations, slowDowns, invalidGrants, limited, unauthorized int
	allStale                                                   chan struct{}
	staleRequests                                              atomic.Int32
}

func TestSeparateProcessesShareAProfileWithoutRefreshRace(t *testing.T) {
	config := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", config)
	fixture := &sharedProfileServer{
		access: "access-0", refresh: "refresh-0",
		expires:  time.Now().Add(15 * time.Minute),
		issued:   map[string]bool{"refresh-0": false},
		allStale: make(chan struct{}),
	}
	const origin = "https://shared.example.invalid"
	for _, round := range []struct {
		number, unauthorized, limited int
	}{{1, 0, 0}, {2, sharedProfileProcesses, 1}, {3, sharedProfileProcesses, 1}} {
		fixture.prepareRound(t, origin, round.number)
		runSharedProfileRound(t, config, round.number, fixture.serveHTTP)
		fixture.checkRound(t, round.number, round.unauthorized, round.limited)
		if t.Failed() {
			return
		}
	}
}

func (server *sharedProfileServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/oauth/token" {
		server.refreshToken(w, r)
		return
	}
	server.mu.Lock()
	valid := !server.revoked && time.Now().Before(server.expires) && r.Header.Get("Authorization") == "Bearer "+server.access
	waitForStale := !valid && server.rotations == 0 && r.URL.Path == "/api/v1/commands"
	if !valid {
		server.unauthorized++
	}
	server.mu.Unlock()

	if !valid {
		if waitForStale {
			// Even late starters must have read the old pair
			// before the first 401 can trigger rotation.
			if server.staleRequests.Add(1) == sharedProfileProcesses {
				close(server.allStale)
			}
			select {
			case <-server.allStale:
			case <-r.Context().Done():
				return
			}
		}
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if r.URL.Path != "/api/v1/commands" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Valiido-Tenant", "12345678")
	w.Header().Set("Valiido-Scope", agentScope)
	time.Sleep(5 * time.Second)
	writeCommandResult(w, resourcePage{Type: "tasks", Resources: []resource{}, Complete: true})
}

func (server *sharedProfileServer) refreshToken(w http.ResponseWriter, r *http.Request) {
	raw := r.FormValue("refresh_token")
	server.mu.Lock()
	if server.rateLimitNext {
		server.rateLimitNext = false
		server.limited++
		server.mu.Unlock()
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":"slow_down"}`)
		return
	}
	used, known := server.issued[raw]
	switch {
	case !known, used, server.revoked:
		if known {
			server.revoked = true
		}
		server.invalidGrants++
		server.mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":"invalid_grant"}`)
	case time.Until(server.expires) > 2*time.Minute:
		server.slowDowns++
		server.mu.Unlock()
		time.Sleep(5 * time.Second)
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":"slow_down"}`)
	default:
		server.issued[raw] = true
		server.rotations++
		server.access = fmt.Sprintf("access-%d", server.rotations)
		server.refresh = fmt.Sprintf("refresh-%d", server.rotations)
		server.issued[server.refresh] = false
		server.expires = time.Now().Add(15 * time.Minute)
		tokens := tokenResponse{AccessToken: server.access, RefreshToken: server.refresh, TokenType: "Bearer", Scope: agentScope}
		server.mu.Unlock()
		// Rails has already invalidated the old access token, but
		// the CLI cannot publish the new credentials until delivery.
		time.Sleep(10 * time.Second)
		server.mu.Lock()
		tokens.ExpiresIn = int64(time.Until(server.expires) / time.Second)
		server.mu.Unlock()
		json.NewEncoder(w).Encode(tokens)
	}
}

func (server *sharedProfileServer) prepareRound(t *testing.T, origin string, round int) {
	t.Helper()
	s, unlock, err := openStore("shared", true)
	if err != nil {
		t.Fatal(err)
	}
	var value session
	if round == 1 {
		value = session{Server: origin, TenantNumber: "12345678", Scope: agentScope, AccessToken: server.access, RefreshToken: server.refresh}
	} else {
		value, err = s.load()
		if err != nil {
			unlock()
			t.Fatal(err)
		}
	}
	value.ExpiresAt = time.Now().Add(-time.Hour)
	value.RefreshAfter = nil
	server.mu.Lock()
	if round > 1 {
		server.expires = time.Now().Add(-time.Minute)
	}
	if round == 2 {
		// Simulate the opposite clock skew: the CLI thinks the
		// token is fresh and must recover from a server-side 401.
		value.ExpiresAt = time.Now().Add(15 * time.Minute)
		server.rateLimitNext = true
	}
	server.mu.Unlock()
	err = s.save(value)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
}

func runSharedProfileRound(t *testing.T, config string, round int, handler http.HandlerFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), commandRequestBudget)
	results := make(chan error, sharedProfileProcesses)
	for range sharedProfileProcesses {
		go func() { results <- runSharedProfileProcess(ctx, config, handler) }()
	}
	for range sharedProfileProcesses {
		if err := <-results; err != nil {
			t.Errorf("round %d: %v", round, err)
		}
	}
	cancel()
}

func (server *sharedProfileServer) checkRound(t *testing.T, round, wantUnauthorized, wantLimited int) {
	t.Helper()
	server.mu.Lock()
	if server.revoked || server.rotations != round-1 || server.slowDowns != 1 || server.invalidGrants != 0 ||
		server.limited != wantLimited || server.unauthorized != wantUnauthorized {
		t.Errorf("round %d: rotations=%d slow_down=%d invalid_grant=%d revoked=%t 429=%d 401=%d",
			round, server.rotations, server.slowDowns, server.invalidGrants, server.revoked, server.limited, server.unauthorized)
	}
	server.mu.Unlock()
}

func runSharedProfileProcess(ctx context.Context, config string, handler http.HandlerFunc) error {
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCLIProcessHelper$")
	command.Env = append(os.Environ(), "VALIIDO_CLI_PROCESS_HELPER=1", "XDG_CONFIG_HOME="+config)
	requests, err := command.StderrPipe()
	if err != nil {
		return err
	}
	replies, err := command.StdinPipe()
	if err != nil {
		return err
	}
	var out bytes.Buffer
	command.Stdout = &out
	if err := command.Start(); err != nil {
		return err
	}
	reader := bufio.NewReader(requests)
	for {
		request, err := http.ReadRequest(reader)
		if err != nil {
			break
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request.WithContext(ctx))
		request.Body.Close()
		response := recorder.Result()
		response.ContentLength = int64(recorder.Body.Len())
		if err := response.Write(replies); err != nil {
			break
		}
	}
	replies.Close()
	err = command.Wait()
	var result struct {
		OK    bool      `json:"ok"`
		Error *apiError `json:"error"`
	}
	if json.Unmarshal(out.Bytes(), &result) != nil {
		return fmt.Errorf("invalid CLI response: %v", err)
	}
	if err == nil && result.OK {
		return nil
	}
	code := ""
	if result.Error != nil {
		code = result.Error.Code
	}
	return fmt.Errorf("CLI failed with code %q: %v", code, err)
}

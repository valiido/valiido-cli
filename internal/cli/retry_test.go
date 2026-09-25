package cli

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRateLimitRetryReportsProgressOnStderr(t *testing.T) {
	defer func(base time.Duration) { retryBackoffBase = base }(retryBackoffBase)
	retryBackoffBase = 0
	requests := 0
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		io.WriteString(w, `{}`)
	})

	c, _ := newClient(server.URL, "token")
	var log bytes.Buffer
	c.retryLog = &log
	if err := c.json(context.Background(), "GET", "/test", nil, true, false, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "Rate limited; retrying") {
		t.Fatalf("missing retry progress: %q", log.String())
	}
}

func TestRateLimitRetryPreservesPostAndAuthorization(t *testing.T) {
	defer func(base time.Duration) { retryBackoffBase = base }(retryBackoffBase)
	retryBackoffBase = 0
	var requests int
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
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
	})

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
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusTooManyRequests)
	})

	c, _ := newClient(server.URL, "secret")
	resp, err := c.requestAPI(context.Background(), http.MethodGet, "/limited", nil, true, nil)
	if err != nil || resp == nil || requests != maxRequestRetries+1 {
		t.Fatalf("retry bound failed: requests=%d response=%v err=%v", requests, resp, err)
	}
	resp.Body.Close()

	retryBackoffBase = time.Second
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.requestAPI(ctx, http.MethodGet, "/limited", nil, true, nil); err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("cancelled retry returned %v", err)
	}
}

func TestTokenRetryPreservesRateLimitWhenBackoffIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c, err := newClient("https://example.invalid", "")
	if err != nil {
		t.Fatal(err)
	}
	attempts := 0
	policy := apiRetryPolicy(http.MethodPost, apiPaths["oauth#token"])
	_, err = c.withRetry(ctx, policy, time.Second, func() (*http.Response, error) {
		attempts++
		rec := httptest.NewRecorder()
		rec.Header().Set("Retry-After", "60")
		rec.WriteHeader(http.StatusTooManyRequests)
		cancel()
		return rec.Result(), nil
	})
	var ae *apiError
	if attempts != 1 || !errors.As(err, &ae) || ae.Code != "rate_limited" {
		t.Fatalf("expected a definitive 429 after one attempt, got %d attempts and %v", attempts, err)
	}
}

func noRetryWait(t *testing.T) {
	t.Helper()
	base := retryBackoffBase
	retryBackoffBase = 0
	t.Cleanup(func() { retryBackoffBase = base })
}

func TestFlakyCommandAndDirectUpload(t *testing.T) {
	noRetryWait(t)
	data := []byte("%PDF-1.4\nSynthetic test\n%%EOF\n")
	path := filepath.Join(t.TempDir(), "synthetic.pdf")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	var prepare, confirm, allocate, put atomic.Int32
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case apiPaths["api/commands#create"]:
			if prepare.Add(1) == 1 {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			writeCommandResult(w, map[string]any{"confirmation_token": "synthetic-confirmation"})
		case apiPaths["api/commands#confirm"]:
			if confirm.Add(1) == 1 {
				w.WriteHeader(http.StatusGatewayTimeout)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		case apiPaths["api/uploads#create"]:
			allocate.Add(1)
			checksum := md5.Sum(data)
			if r.FormValue("checksum") != base64.StdEncoding.EncodeToString(checksum[:]) {
				t.Error("metadata checksum differs from file")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"signed_id": "synthetic-id", "attachable_sgid": "synthetic-sgid", "filename": "synthetic.pdf",
				"byte_size": len(data), "content_type": "application/pdf",
				"direct_upload": map[string]any{"url": "http://" + r.Host + "/same-blob"},
			})
		case "/same-blob":
			body, err := io.ReadAll(r.Body)
			if err != nil || r.Method != http.MethodPut || !bytes.Equal(data, body) {
				t.Error("upload replay changed bytes or method")
			}
			if put.Add(1) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})

	count := 0
	ctx := context.WithValue(t.Context(), retryKey{}, &count)
	c, _ := newClient(server.URL, "synthetic-bearer")
	if _, err := c.runCommand(ctx, "comments.create", "{}", false); err != nil {
		t.Fatal(err)
	}
	_, err := c.resolveCommand(ctx, "confirm", "synthetic-confirmation")
	var ae *apiError
	if !errors.As(err, &ae) || ae.Code != "not_found" || !strings.Contains(ae.Hint, "resource read") {
		t.Fatalf("ambiguous confirmation lacks recovery hint: %#v", err)
	}
	if _, err := c.uploadFile(ctx, path); err != nil {
		t.Fatal(err)
	}
	if prepare.Load() != 2 || confirm.Load() != 2 || allocate.Load() != 1 || put.Load() != 2 || count != 3 {
		t.Fatalf("unexpected attempts: prepare %d, confirm %d, allocate %d, PUT %d, retried %d",
			prepare.Load(), confirm.Load(), allocate.Load(), put.Load(), count)
	}
}

func TestTransportReplayPolicy(t *testing.T) {
	noRetryWait(t)
	for _, tc := range []struct {
		name, method, operation string
		attempts                int
	}{
		{"read", "GET", "api/identities#show", 4},
		{"prepare", "POST", "api/commands#create", 4},
		{"confirm", "POST", "api/commands#confirm", 4},
		{"reject", "POST", "api/commands#reject", 4},
		{"allocate", "POST", "api/uploads#create", 1},
		{"rotate", "POST", "oauth#token", 1},
		{"revoke", "POST", "oauth#revoke", 4},
		{"pair", "POST", "oauth#device_authorization", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newClient("https://example.invalid", "synthetic")
			attempts := 0
			c.http.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
				attempts++
				return nil, io.ErrUnexpectedEOF
			})
			count := 0
			ctx := context.WithValue(t.Context(), retryKey{}, &count)
			err := c.json(ctx, tc.method, apiPaths[tc.operation], nil, true, false, nil)
			if attempts != tc.attempts || count != attempts-1 || err == nil || !strings.Contains(err.Error(), "response was complete") {
				t.Fatalf("attempts=%d retried=%d error=%v", attempts, count, err)
			}
		})
	}
}

func TestRetryInterruptedResponseAndRetryAfter(t *testing.T) {
	noRetryWait(t)
	c, _ := newClient("https://example.invalid", "synthetic")
	attempts := 0
	c.http.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
		attempts++
		rec := httptest.NewRecorder()
		if attempts == 1 {
			resp := rec.Result()
			resp.Body = io.NopCloser(errorReader{})
			return resp, nil
		}
		_, _ = rec.WriteString(`{"synthetic":true}`)
		return rec.Result(), nil
	})
	var result map[string]any
	if err := c.json(t.Context(), "GET", "/test", nil, true, false, &result); err != nil || attempts != 2 || result["synthetic"] != true {
		t.Fatalf("attempts=%d result=%v err=%v", attempts, result, err)
	}
	rec := httptest.NewRecorder()
	rec.Header().Set("Retry-After", "45")
	if got := retryAfterDelay(rec.Result(), 0); got != 45*time.Second {
		t.Fatalf("seconds Retry-After = %v", got)
	}
	rec = httptest.NewRecorder()
	rec.Header().Set("Retry-After", time.Now().Add(time.Minute).UTC().Format(http.TimeFormat))
	if got := retryAfterDelay(rec.Result(), 0); got < 58*time.Second || got > time.Minute {
		t.Fatalf("HTTP-date Retry-After = %v", got)
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestNetworkDiagnosticsDoNotRevealURL(t *testing.T) {
	c, _ := newClient("https://example.invalid", "synthetic")
	c.http.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
		return nil, &net.DNSError{Err: "hidden synthetic secret", Name: r.URL.Host, IsNotFound: true}
	})
	err := c.json(t.Context(), "GET", "/hidden-synthetic-secret", nil, true, false, nil)
	if err == nil || err.Error() != "DNS resolution failed." {
		t.Fatalf("unexpected diagnostic %v", err)
	}
}

func TestServiceUnavailableResponseHidesBodyAndCountsRetries(t *testing.T) {
	noRetryWait(t)
	c, _ := newClient("https://example.invalid", "synthetic")
	c.http.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		rec.WriteHeader(http.StatusServiceUnavailable)
		_, _ = rec.WriteString(`{"error":{"code":"synthetic-secret","message":"private synthetic response"}}`)
		return rec.Result(), nil
	})
	count := 0
	ctx := context.WithValue(t.Context(), retryKey{}, &count)
	err := c.json(ctx, "GET", "/test", nil, true, false, nil)
	var ae *apiError
	if !errors.As(err, &ae) || ae.Code != "server_unavailable" || ae.Message != "Server returned HTTP 503." || count != 3 {
		t.Fatalf("missing sanitized HTTP cause or retry count: %v retried=%d", err, count)
	}
}

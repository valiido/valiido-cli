package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
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
		{http.StatusConflict, `{"error":{"code":"duplicate","message":"A record with these unique attributes already exists."}}`, "duplicate", 9},
		{http.StatusUnprocessableEntity, `{"error":{"code":"invalid_input","message":"Input is invalid."}}`, "invalid_input", 6},
		{http.StatusUnprocessableEntity, `{}`, "invalid_input", 6},
		{http.StatusTooManyRequests, `{}`, "rate_limited", 7},
	}
	for _, test := range tests {
		t.Run(strconv.Itoa(test.status), func(t *testing.T) {
			server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.status)
				io.WriteString(w, test.body)
			})

			c, _ := newClient(server.URL, "token")
			err := c.json(context.Background(), "GET", "/test", nil, true, false, nil)
			var apiErr *apiError
			if !errors.As(err, &apiErr) || apiErr.Code != test.code || exitCode(apiErr.Code) != test.exit {
				t.Fatalf("status %d mapped to %#v", test.status, err)
			}
		})
	}
}

func TestAPIErrorPreservesStructuredDetails(t *testing.T) {
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		io.WriteString(w, `{"error":{"code":"validation_failed","message":"Two rows failed."},"data":{"errors":[{"index":2},{"index":4}]}}`)
	})

	c, _ := newClient(server.URL, "token")
	err := c.json(context.Background(), "POST", "/test", nil, true, false, nil)
	var apiErr *apiError
	if !errors.As(err, &apiErr) || apiErr.Details == nil {
		t.Fatalf("structured details were lost: %#v", err)
	}
	errorsList, ok := apiErr.Details["errors"].([]any)
	if !ok || len(errorsList) != 2 {
		t.Fatalf("unexpected details: %#v", apiErr.Details)
	}
}

func TestRedirectDoesNotLeakCredential(t *testing.T) {
	called := false
	other := newTestServer(t, func(w http.ResponseWriter, r *http.Request) { called = true })

	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, 302) })

	c, _ := newClient(server.URL, "secret")
	_, err := c.me(context.Background())
	if err == nil || called {
		t.Fatal("followed credential-bearing redirect")
	}
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

func TestTokenScopeAndHTTPErrorRedaction(t *testing.T) {
	if validateToken(tokenResponse{AccessToken: "x", RefreshToken: "y", TokenType: "Bearer", Scope: "admin", ExpiresIn: 900}, "") == nil {
		t.Fatal("accepted admin scope")
	}
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		io.WriteString(w, `{"error":"invalid_grant","error_description":"secret-token"}`)
	})

	c, _ := newClient(server.URL, "")
	err := c.json(context.Background(), "POST", "/oauth/token", url.Values{}, false, true, nil)
	if err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatal("leaked server error")
	}
}

func writeCommandResult(w http.ResponseWriter, data any) {
	encoded, _ := json.Marshal(data)
	var object map[string]any
	_ = decodeJSON(encoded, &object)
	json.NewEncoder(w).Encode(commandResult{OK: true, Data: object, Summary: "Done"})
}

type testTransport func(*http.Request) (*http.Response, error)

func (f testTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func saveTestSession(t *testing.T, profile string, value session) {
	t.Helper()
	store, unlock, err := openStore(profile, true)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := store.save(value); err != nil {
		t.Fatal(err)
	}
}

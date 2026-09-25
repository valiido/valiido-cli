package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCommandCatalogFiltersAreEncoded(t *testing.T) {
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/commands" || r.URL.Query().Get("query") != "asset create" || r.URL.Query().Get("limit") != "7" {
			t.Fatalf("unexpected catalog request: %s", r.URL.String())
		}
		json.NewEncoder(w).Encode(commandCatalog{Commands: []map[string]any{}, Complete: true})
	})

	c, _ := newClient(server.URL, "secret")
	if _, err := c.commands(context.Background(), "asset create", 7); err != nil {
		t.Fatal(err)
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

func TestCommandCatalogLimitAndBreadcrumb(t *testing.T) {
	c, err := newClient("http://127.0.0.1:8080", "test-token")
	if err != nil {
		t.Fatal(err)
	}
	requests := 0
	c.http.Transport = testTransport(func(req *http.Request) (*http.Response, error) {
		requests++
		query := req.URL.Query()
		if query.Get("query") != "asset create" || query.Get("limit") != "1" {
			t.Fatalf("unexpected catalog query: %v", query)
		}
		w := httptest.NewRecorder()
		json.NewEncoder(w).Encode(commandCatalog{
			Commands: []map[string]any{{"name": "assets.create"}},
			Count:    1, TotalCount: 3,
		})
		return w.Result(), nil
	})
	catalog, err := c.commands(t.Context(), "asset create", 1)
	if err != nil || requests != 1 || catalog.Count != 1 || catalog.Complete {
		t.Fatalf("unexpected catalog: %+v, requests=%d, err=%v", catalog, requests, err)
	}
	var buf bytes.Buffer
	out := &output{Writer: &buf, profile: "test"}
	if err := moreResults(out, catalog, "command list", "asset create"); err != nil {
		t.Fatal(err)
	}
	var result response
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	want := "valiido command list --query 'asset create' --profile 'test'"
	if len(result.Breadcrumbs) != 1 || result.Breadcrumbs[0].Action != "load_more" || result.Breadcrumbs[0].Command != want {
		t.Fatalf("unexpected catalog breadcrumb: %+v", result.Breadcrumbs)
	}
}

package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestResourceAliases(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"documents read 12", "documents show 12"},
		{"documents show 12", "documents show 12"},
		{"risks list", "risks list"},
		{"assets read 12", "resource show assets 12"},
		{"tasks create", "command run tasks.create"},
		{"change-requests show 45", "resource show change_managements 45"},
		{"tasks delete 7 --dry-run", `command run tasks.delete --input {"id":7} --dry-run`},
		{"tasks show 0", "tasks show 0"},
		{"assets show 7 --input {}", "command run assets.show 7 --input {}"},
		{"comments create", "command run comments.create"},
		{"document upload synthetic.pdf", "documents upload synthetic.pdf"},
		{"command confirm synthetic", "commands confirm synthetic"},
		{"resource list assets", "resources list assets"},
		{"assethandovers items list", "resource list assethandover_items"},
		{"auditmagic list-objects", "command run auditmagic.list_objects"},
		{"tasks toggle-complete 7", `command run tasks.toggle_complete --input {"id":7}`},
	} {
		if got := strings.Join(resourceShortcut(strings.Fields(tc.input)), " "); got != tc.want {
			t.Errorf("%s = %s, want %s", tc.input, got, tc.want)
		}
	}

}

func TestResourceShortcutUsesStableCommandPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const tenant = "12345678"
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case apiPaths["api/identities#show"]:
			_ = json.NewEncoder(w).Encode(identity{TenantNumber: tenant, Scope: agentScope})
		case apiPaths["api/commands#create"]:
			if r.FormValue("name") != "comments.create" || r.FormValue("input") != `{"resource_id":12}` {
				t.Error("wrong command or input")
			}
			writeCommandResult(w, map[string]any{"pending_confirmation": true, "confirmation_token": "synthetic-token"})
		default:
			t.Error("unexpected path")
		}
	})

	saveTestSession(t, "synthetic", session{
		Server: server.URL, TenantNumber: tenant, Scope: agentScope, AccessToken: "synthetic-bearer",
		RefreshToken: "synthetic-refresh", ExpiresAt: time.Now().Add(time.Hour),
	})
	var alias, original bytes.Buffer
	flags := []string{"--profile", "synthetic", "--input", `{"resource_id":12}`, "--json"}
	for _, tc := range []struct {
		args []string
		out  *bytes.Buffer
	}{
		{[]string{"comments", "create"}, &alias},
		{[]string{"command", "run", "comments.create"}, &original},
	} {
		if code := Run(t.Context(), append(tc.args, flags...), tc.out, io.Discard); code != 0 {
			t.Fatalf("%v exit=%d: %s", tc.args, code, tc.out)
		}
	}
	if !bytes.Equal(alias.Bytes(), original.Bytes()) || !strings.Contains(alias.String(), `"pending_confirmation":true`) {
		t.Fatalf("responses differ: %s %s", alias.String(), original.String())
	}
}

func TestSpecialResourceReadUsesCatalogShape(t *testing.T) {
	c, _ := newClient("https://example.invalid", "synthetic")
	requests := 0
	c.http.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
		requests++
		rec := httptest.NewRecorder()
		switch r.Method {
		case "GET":
			if r.URL.Query().Get("query") != "synthetic_feed.show" {
				t.Fatal("catalog lookup missed the action")
			}
			_ = json.NewEncoder(rec).Encode(commandCatalog{
				Count: 1, TotalCount: 1, Complete: true,
				Commands: []map[string]any{{
					"name": "synthetic_feed.show", "input_schema": map[string]any{
						"id": map[string]any{"type": "integer", "required": true, "description": "Entry position"},
					},
				}},
			})
		case "POST":
			if r.FormValue("input") != `{"id":12}` {
				t.Fatal("wrong read input")
			}
			writeCommandResult(rec, map[string]any{"id": 12, "type": "synthetic_feed", "label": "Synthetic entry"})
		}
		return rec.Result(), nil
	})
	result, handled, err := c.specialResourceRead(t.Context(), "synthetic_feed.show", 12, listOptions{})
	if err != nil || !handled || requests != 2 || result.Data["label"] != "Synthetic entry" {
		t.Fatalf("special read failed: requests=%d handled=%t result=%v err=%v", requests, handled, result, err)
	}
}

func TestSpecialListsTakePagingFlags(t *testing.T) {
	var inputs []string
	fakeProfile(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			_ = json.NewEncoder(w).Encode(commandCatalog{Count: 1, TotalCount: 1, Complete: true,
				Commands: []map[string]any{{"name": "auditmagic.list_objects", "input_schema": map[string]any{
					"resource_type": map[string]any{"type": "string"}, "page": map[string]any{"type": "integer"},
				}}}})
			return
		}
		inputs = append(inputs, r.FormValue("input"))
		var input struct{ Page int }
		_ = json.Unmarshal([]byte(r.FormValue("input")), &input)
		page := max(input.Page, 1)
		rows := []any{map[string]any{"type": "assets", "id": page*10 + 1}, map[string]any{"type": "assets", "id": page*10 + 2}}
		var next any
		if page < 3 {
			next = page + 1
		}
		writeCommandResult(w, map[string]any{"resources": rows, "next_page": next})
	})
	var stdout bytes.Buffer
	args := []string{"auditmagic", "list-objects", "--input", `{"resource_type":"assets"}`, "--limit", "3", "--cursor", "2", "--profile", "work", "--json"}
	if code := Run(t.Context(), args, &stdout, io.Discard); code != 0 {
		t.Fatalf("paging failed with %d: %s", code, &stdout)
	}
	var envelope struct {
		Data struct {
			Resources  []map[string]any
			Count      int
			Complete   bool
			NextCursor string `json:"next_cursor"`
		}
		Breadcrumbs []breadcrumb
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	want := []string{`{"page":2,"resource_type":"assets"}`, `{"page":3,"resource_type":"assets"}`}
	if strings.Join(inputs, " ") != strings.Join(want, " ") || envelope.Data.Count != 4 || !envelope.Data.Complete {
		t.Fatalf("unexpected pages %v: %s", inputs, &stdout)
	}

	inputs = nil
	stdout.Reset()
	if code := Run(t.Context(), []string{"auditmagic", "list_objects", "--limit", "1", "--profile", "work", "--json"}, &stdout, io.Discard); code != 0 {
		t.Fatalf("paging failed with %d: %s", code, &stdout)
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil || len(inputs) != 1 || envelope.Data.NextCursor != "2" ||
		!slices.ContainsFunc(envelope.Breadcrumbs, func(crumb breadcrumb) bool {
			return crumb.Command == "valiido auditmagic list-objects --profile 'work' --cursor '2' --limit 1"
		}) {
		t.Fatalf("missing continuation: %v %s", inputs, &stdout)
	}

	stdout.Reset()
	if code := Run(t.Context(), []string{"auditmagic", "list-objects", "--page-size", "5", "--json"}, &stdout, io.Discard); code != 2 {
		t.Fatalf("accepted --page-size: %s", &stdout)
	}
}

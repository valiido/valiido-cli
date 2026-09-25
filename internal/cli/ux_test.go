package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func fakeProfile(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	originalTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	http.DefaultTransport = testTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "example.invalid" || r.Header.Get("Authorization") != "Bearer synthetic-access" {
			t.Fatalf("unexpected origin or credentials")
		}
		w := httptest.NewRecorder()
		if r.URL.Path == apiPaths["api/identities#show"] {
			_ = json.NewEncoder(w).Encode(identity{TenantNumber: "12345678", Scope: agentScope})
		} else {
			handler(w, r)
		}
		return w.Result(), nil
	})
	saveTestSession(t, "work", session{
		Server: "https://example.invalid", TenantNumber: "12345678", Scope: agentScope,
		AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", ExpiresAt: time.Now().Add(time.Hour),
	})
}

func TestUXReadAndOpenShareRailsURL(t *testing.T) {
	const id = 9007199254740993
	const route = "/12345678/isms/documents/9007199254740993"
	item := documentResource(id, "rich_text")
	item.URL = route
	item.Attributes["parent_id"] = int64(id + 1)
	item.Attributes["content"] = "<p>Synthetic</p>"
	fakeProfile(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.FormValue("name") != "documents.show" ||
			r.FormValue("input") != `{"id":9007199254740993}` {
			t.Fatalf("unexpected request")
		}
		writeCommandResult(w, item)
	})
	for _, command := range []string{"documents show", "open documents"} {
		args := append(strings.Fields(command), fmt.Sprint(id), "--profile", "work", "--json")
		var stdout bytes.Buffer
		if code := Run(t.Context(), args, &stdout, io.Discard); code != 0 {
			t.Fatalf("%v failed with %d: %s", args, code, &stdout)
		}
		var envelope struct {
			URL         string
			Data        json.RawMessage
			Breadcrumbs []breadcrumb
		}
		if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil || envelope.URL != "https://example.invalid"+route {
			t.Fatalf("missing trusted URL: %s", &stdout)
		}
		if command == "documents show" {
			if !bytes.Contains(envelope.Data, []byte(`"parent_id":9007199254740994`)) || bytes.Contains(envelope.Data, []byte(`"url"`)) ||
				!bytes.Contains(envelope.Data, []byte(`"content":"\u003cp\u003eSynthetic\u003c/p\u003e"`)) {
				t.Fatalf("changed document contract: %s", envelope.Data)
			}
			if !slices.ContainsFunc(envelope.Breadcrumbs, func(crumb breadcrumb) bool {
				return crumb.Command == "valiido open documents 9007199254740993 --profile work"
			}) {
				t.Fatalf("missing browser breadcrumb: %+v", envelope.Breadcrumbs)
			}
		}
	}
}

func TestUXShowIsReadableInTerminal(t *testing.T) {
	document := documentResource(42, "rich_text")
	document.Attributes["content"] = "<div><h1>Synthetic policy</h1><p>Keep &amp; review.</p></div>"
	document.Attributes["content_text"] = "Synthetic policy Keep & review."
	task := resource{Type: "tasks", ID: 7, Version: resourceVersion(1), Label: "Synthetic task",
		Attributes: map[string]any{"title": "Synthetic task", "content": "<div>Check <strong>backups</strong></div>"}}
	fakeProfile(t, func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("name") == "tasks.show" {
			writeCommandResult(w, task)
			return
		}
		writeCommandResult(w, document)
	})
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"documents", "show", "42"}, "content text  Synthetic policy Keep & review."},
		{[]string{"documents", "read", "42"}, "title  Document"},
		{[]string{"tasks", "show", "7"}, "content  Check backups"},
	} {
		var stdout bytes.Buffer
		args := append(tc.args, "--profile", "work", "--styled")
		if code := Run(t.Context(), args, &stdout, io.Discard); code != 0 ||
			strings.Contains(stdout.String(), `"ok"`) || strings.Contains(stdout.String(), "<div>") ||
			!strings.Contains(stdout.String(), tc.want) {
			t.Errorf("%v printed %d: %s", tc.args, code, &stdout)
		}
	}
}

func TestUXFullAuditsWithoutVersionOrTitle(t *testing.T) {
	audit := map[string]any{"type": "auditmagic_fullaudits", "id": 12, "label": "2026-11-18 10:00",
		"attributes": map[string]any{"analysis_timestamp": "2026-11-18T10:00:00Z", "rating": "good", "summary": "Synthetic summary"}}
	fakeProfile(t, func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("name") == "auditmagic_fullaudits.list" {
			writeCommandResult(w, map[string]any{"type": "auditmagic_fullaudits", "resources": []any{audit}, "count": 1, "complete": true})
			return
		}
		writeCommandResult(w, audit)
	})
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"full-audits", "show", "12"}, []string{"rating  good", "analysis timestamp  2026-11-18T10:00:00Z"}},
		{[]string{"full-audits", "read", "12"}, []string{"rating  good"}},
		{[]string{"full-audits", "list"}, []string{"STATUS", "good", "2026-11-18T10:00:00Z"}},
	} {
		var stdout bytes.Buffer
		if code := Run(t.Context(), append(tc.args, "--profile", "work", "--styled"), &stdout, io.Discard); code != 0 {
			t.Fatalf("%v failed with %d: %s", tc.args, code, &stdout)
		}
		for _, want := range tc.want {
			if !strings.Contains(stdout.String(), want) {
				t.Errorf("%v omitted %q: %s", tc.args, want, &stdout)
			}
		}
	}
}

func TestUXRejectsUntrustedRecordLinks(t *testing.T) {
	const origin = "https://example.invalid"
	const tenant = "12345678"
	const valid = "/12345678/isms/documents/42"
	for _, raw := range []string{valid, origin + valid} {
		if recordURL(origin, tenant, raw) != origin+valid {
			t.Errorf("rejected %q", raw)
		}
	}
	for _, raw := range []string{
		"", "//elsewhere.invalid" + valid, "https://elsewhere.invalid" + valid,
		"http://example.invalid" + valid, "https://user@example.invalid" + valid,
		"/87654321/isms/documents/42", "/12345678/isms/../other", valid + "?key=hidden",
		"/12345678/isms/%2e%2e/other", valid + "%2F..", valid + "#fragment",
	} {
		if recordURL(origin, tenant, raw) != "" {
			t.Errorf("accepted %q", raw)
		}
	}
}

func TestUXConfirmationPipe(t *testing.T) {
	const token = "synthetic-confirmation"
	resolutions := 0
	fakeProfile(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case apiPaths["api/commands#create"]:
			writeCommandResult(w, map[string]any{"pending_confirmation": true, "confirmation_token": token,
				"input": map[string]string{"title": "Synthetic title"}})
		case apiPaths["api/commands#confirm"], apiPaths["api/commands#reject"]:
			resolutions++
			if got := r.FormValue("confirmation_token"); got != token {
				t.Fatal("confirmation token changed")
			}
			writeCommandResult(w, map[string]bool{"processed": true})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	})

	var stdout bytes.Buffer
	args := []string{"tasks", "update", "--input", `{"id":42}`, "--profile", "work"}
	if code := Run(t.Context(), append(slices.Clone(args), "--styled"), &stdout, io.Discard); code != 0 ||
		strings.Contains(stdout.String(), token) || !strings.Contains(stdout.String(), "Synthetic title") {
		t.Fatalf("unexpected styled preparation: %s", &stdout)
	}
	for _, action := range []string{"confirm", "reject"} {
		stdout.Reset()
		if code := Run(t.Context(), append(slices.Clone(args), "--json"), &stdout, io.Discard); code != 0 ||
			!strings.Contains(stdout.String(), "valiido command confirm - --profile work") {
			t.Fatalf("missing confirmation breadcrumb: %s", &stdout)
		}
		file, err := os.CreateTemp(t.TempDir(), "preparation")
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		if _, err := file.Write(stdout.Bytes()); err != nil {
			t.Fatal(err)
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		stdin := os.Stdin
		os.Stdin = file
		code := Run(t.Context(), []string{"command", action, "-", "--profile", "work"}, io.Discard, io.Discard)
		os.Stdin = stdin
		if code != 0 {
			t.Fatalf("%s returned %d", action, code)
		}
	}
	if resolutions != 2 {
		t.Fatalf("expected two resolutions, got %d", resolutions)
	}
	for _, input := range []string{token, fmt.Sprintf("%q", token), `{"confirmation_token":"` + token + `"}`} {
		if got, err := readConfirmation(t.Context(), strings.NewReader(input)); err != nil || got != token {
			t.Fatalf("confirmation stdin failed: %v", err)
		}
	}
	for _, input := range []string{`{"ok":false,"data":{"confirmation_token":"invalid"}}`, "[]", `{"data":{"batches":[]}}`} {
		if _, err := readConfirmation(t.Context(), strings.NewReader(input)); err == nil {
			t.Fatal("accepted a failed or ambiguous preparation")
		}
	}
}

func TestUXPreviewsDoNotWrite(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	fakeProfile(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/api/v1/resources/assets/42/pdf" {
			t.Fatalf("unexpected preview request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/pdf")
		fmt.Fprint(w, "%PDF-1.7 synthetic")
	})
	input := filepath.Join(t.TempDir(), "synthetic.pdf")
	if err := os.WriteFile(input, []byte("%PDF-1.7 synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "empty.pdf")
	for _, args := range [][]string{
		{"auth", "login", "--profile", "work", "--server", "https://example.invalid"},
		{"auth", "logout", "--profile", "work"},
		{"setup", "--agent", "all"},
		{"documents", "upload", input},
		{"assets", "pdf", "42", "--output", destination, "--profile", "work"},
	} {
		var stdout bytes.Buffer
		if code := Run(t.Context(), append(args, "--dry-run", "--json"), &stdout, io.Discard); code != 0 ||
			!strings.Contains(stdout.String(), `"dry_run":true`) {
			t.Errorf("%v returned %d: %s", args, code, &stdout)
		}
	}
	for _, path := range []string{destination, filepath.Join(home, ".agents"), filepath.Join(home, ".claude")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("preview created %s", path)
		}
	}
}

func TestUXFailureModesAndInterruption(t *testing.T) {
	var stdout, stderr bytes.Buffer
	args := []string{"documents", "read", "0", "--styled"}
	if code := Run(t.Context(), args, &stdout, &stderr); code != 2 ||
		stdout.Len() != 0 || !strings.Contains(stderr.String(), "Next   valiido documents read --help") {
		t.Fatalf("unexpected human error: %d %q %q", code, &stdout, &stderr)
	}
	stdout.Reset()
	stderr.Reset()
	args = []string{"documents", "list", "--limit", "invalid-value", "--json"}
	if code := Run(t.Context(), args, &stdout, &stderr); code != 2 || stderr.Len() != 0 ||
		!strings.Contains(stdout.String(), `"hint":`) || !strings.Contains(stdout.String(), `"action":"recover"`) ||
		!strings.Contains(stdout.String(), "Invalid value for --limit") || strings.Contains(stdout.String(), "invalid-value") {
		t.Fatalf("unexpected machine error: %d %q %q", code, &stdout, &stderr)
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	stdout.Reset()
	if code := Run(ctx, []string{"auth", "login", "--json"}, &stdout, &stderr); code != 1 ||
		!strings.Contains(stdout.String(), `"code":"cancelled"`) {
		t.Fatalf("unexpected interrupt: %d %q", code, &stdout)
	}
}

func TestUXHelpCoversRoutedResourceActions(t *testing.T) {
	for _, resource := range resourceNames {
		for _, action := range append(resourceActions, specialActions[resource.server]...) {
			args := append(strings.Fields(resource.path), action.name, "--help", "--json")
			var stdout bytes.Buffer
			if code := Run(t.Context(), args, &stdout, io.Discard); code != 0 ||
				!strings.Contains(stdout.String(), "valiido "+resource.path+" "+displayVerb(action.name)+" ") {
				t.Errorf("%v returned %d: %s", args, code, &stdout)
			}
		}
	}
	for noun, actions := range specialActions {
		for _, action := range actions {
			for _, verb := range []string{action.name, displayVerb(action.name)} {
				var stdout bytes.Buffer
				if code := Run(t.Context(), []string{noun, verb, "--help"}, &stdout, io.Discard); code != 0 ||
					!strings.HasPrefix(stdout.String(), action.description+"\n") ||
					!strings.Contains(stdout.String(), " "+displayVerb(action.name)+" ") {
					t.Errorf("%s %s returned %d: %s", noun, verb, code, &stdout)
				}
			}
		}
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"help", "documents", "read"}, "--output FILE.pdf"},
		{[]string{"assets", "pdf", "--help"}, "$ valiido assets pdf 42 --output synthetic.pdf"},
		{[]string{"resource", "import", "--help"}, "$ valiido resource import documents --file synthetic.json --dry-run"},
		{[]string{"command", "confirm", "--help"}, "read a preparation result"},
	} {
		var stdout bytes.Buffer
		if code := Run(t.Context(), tc.args, &stdout, io.Discard); code != 0 || !strings.Contains(stdout.String(), tc.want) {
			t.Errorf("%v returned %d: %s", tc.args, code, &stdout)
		}
	}
}

func TestUXCompletionUsesSelectedProfileAndBoundsReads(t *testing.T) {
	fakeProfile(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.FormValue("name"); got != "kpi_measurements.list" {
			t.Fatalf("unexpected lookup %s", got)
		}
		var input struct{ Limit int }
		if json.Unmarshal([]byte(r.FormValue("input")), &input) != nil || input.Limit != 25 {
			t.Fatal("completion did not bound the lookup")
		}
		writeCommandResult(w, resourcePage{Type: "kpi_measurements", Complete: true, Resources: []resource{{
			Type: "kpi_measurements", ID: 9007199254740993, Version: resourceVersion(0), Attributes: map[string]any{},
		}}})
	})
	var stdout bytes.Buffer
	if err := complete(t.Context(), &stdout, "open kpis measurements", "work", "9"); err != nil ||
		stdout.String() != "9007199254740993\n" {
		t.Fatalf("bad ID completion: %q (%v)", &stdout, err)
	}
	flags := completionFlags("documents show 42")
	if !slices.Contains(flags, "--text") || !slices.Contains(flags, "--dry-run") || slices.Contains(flags, "--input-file") {
		t.Fatalf("unexpected flags %v", flags)
	}
}

func TestUXSpecialPaginationKeepsLargeCursor(t *testing.T) {
	var stdout bytes.Buffer
	out := &output{Writer: &stdout, kind: "chats", profile: "work"}
	const n = json.Number("9007199254740993")
	result := commandResult{Data: map[string]any{"chats": []any{map[string]any{"id": n}}, "next_before_id": n}}
	if err := specialListSuccess(out, result, listOptions{}); err != nil ||
		!strings.Contains(stdout.String(), "--cursor '9007199254740993'") ||
		!strings.Contains(stdout.String(), "valiido chats show 9007199254740993") {
		t.Fatalf("chat continuation lost the cursor or read path: %s (%v)", &stdout, err)
	}
	result.Breadcrumbs = nil
	result.Data = map[string]any{"next_cursor": n}
	moreComments(out, &result, `{"resource_type":"documents","resource_id":42}`)
	if len(result.Breadcrumbs) != 1 || !strings.Contains(result.Breadcrumbs[0].Command, `"before":9007199254740993`) {
		t.Fatalf("comment continuation missing: %+v", result.Breadcrumbs)
	}
}

func TestUXNarrowUnicodeAndRelativeDates(t *testing.T) {
	for _, label := range []string{"漢字", "👩‍💻文", "🇩🇪文", "a\u0301文"} {
		got := fitDisplay(label, 3)
		if displayWidth(got) > 3 || !strings.HasSuffix(got, "…") && displayWidth(label) > 3 {
			t.Fatalf("invalid clipping of %q: %q", label, got)
		}
	}
	v := &readableView{width: 16, relative: true, now: time.Date(2026, 11, 18, 11, 0, 0, 0, time.UTC)}
	result, err := inspectResult(map[string]any{"resources": []map[string]any{
		{"id": 42000, "label": "説明書 👩‍💻 Übungen", "updated_at": "2026-11-18T10:40:00Z"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	v.writeRows(result, "documents")
	for _, line := range strings.Split(strings.TrimSpace(v.String()), "\n") {
		if displayWidth(line) > v.width {
			t.Errorf("overlong TTY row: %q", line)
		}
	}
	if !strings.Contains(v.String(), "42000") || v.date("2026-11-18T10:40:00Z") != "20m ago" {
		t.Fatalf("lost ID or relative date: %s", v)
	}
}

func TestLeadingGlobalOptions(t *testing.T) {
	fakeProfile(t, func(w http.ResponseWriter, r *http.Request) {
		writeCommandResult(w, documentResource(42, "rich_text"))
	})
	for _, args := range [][]string{
		{"--profile", "work", "--json", "documents", "read", "42"},
		{"--json", "documents", "read", "42", "--profile=work"},
		{"documents", "read", "42", "--profile", "work", "--json"},
	} {
		var out bytes.Buffer
		if code := Run(t.Context(), args, &out, io.Discard); code != 0 || !json.Valid(out.Bytes()) {
			t.Fatalf("global options failed (%d) for %v", code, args)
		}
	}
	if code := Run(t.Context(), []string{"--json", "version", "--json"}, io.Discard, io.Discard); code != 2 {
		t.Fatal("duplicate global option accepted")
	}
}

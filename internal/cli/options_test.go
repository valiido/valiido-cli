package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSearchOptionLimitsFailBeforeReadingCredentials(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, args := range [][]string{
		{"command", "list", "--limit", "0"},
		{"command", "list", "--limit", "1001"},
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

func TestIncompatibleOptionsReturnSpecificUsageMessages(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	tests := []struct {
		args    []string
		message string
	}{
		{[]string{"document", "read", "1", "--text", "--json"}, "Use either --text or --json"},
		{[]string{"resource", "pdf", "assets", "1"}, "--output is required for resource pdf"},
		{[]string{"command", "list", "unexpected"}, "Unexpected positional arguments"},
	}
	for _, test := range tests {
		var out bytes.Buffer
		if code := Run(context.Background(), test.args, &out, io.Discard); code != 2 {
			t.Fatalf("%v returned %d: %s", test.args, code, out.String())
		}
		if !strings.Contains(out.String(), test.message) {
			t.Fatalf("%v returned a vague message: %s", test.args, out.String())
		}
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

func TestExitCodeMappingAndOversizedResourceID(t *testing.T) {
	tests := map[string]int{
		"usage": 2, "auth": 3, "forbidden": 4, "not_authorized": 4, "read_only_record": 4, "not_found": 5,
		"example_not_allowed": 4, "guide_chapter_not_allowed": 4,
		"template_not_found": 5, "example_not_found": 5, "guide_item_not_found": 5,
		"unknown_command": 6, "invalid_input": 6, "validation_failed": 6, "unsupported_resource_attributes": 6,
		"invalid_format": 6, "invalid_file_reference": 6, "placeholder_email_not_allowed": 6, "invalid_cursor": 6,
		"invalid_reference_type": 6, "unknown_resource_type": 6, "unsupported_resource_action": 6,
		"unsupported_delete_strategy": 6, "unsupported_commentable": 6, "ambiguous_assignment": 6,
		"rate_limited": 7, "storage": 8,
		"conflict": 9, "duplicate": 9, "already_copied": 9, "already_exists": 9, "already_has_access": 9, "already_in_progress": 9,
		"framework_not_allowed": 1, "invalid_example_attachment": 1, "api": 1, "": 1,
	}
	for code, want := range tests {
		if got := exitCode(code); got != want {
			t.Errorf("exitCode(%q) = %d, want %d", code, got, want)
		}
	}
	var out bytes.Buffer
	if code := Run(context.Background(), []string{"resource", "read", "assets", "9223372036854775808"}, &out, io.Discard); code != 2 || !strings.Contains(out.String(), "9223372036854775807") {
		t.Fatalf("oversized ID was not a clear usage error: code=%d output=%s", code, out.String())
	}
}

func TestContextualHelp(t *testing.T) {
	for _, args := range [][]string{{"resource", "--help"}, {"command", "run", "--help"}, {"resource", "import", "-h"}} {
		var out bytes.Buffer
		if code := Run(context.Background(), args, &out, io.Discard); code != 0 || !strings.Contains(out.String(), "\nUSAGE\n") {
			t.Fatalf("%v did not render contextual help: code=%d output=%q", args, code, out.String())
		}
	}
}

package cli

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestOfflineHelp(t *testing.T) {
	// A local path that cannot contain a profile.
	t.Setenv("XDG_CONFIG_HOME", "/dev/null")
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"--help"}, []string{"\nUSAGE\n", "\nISMS\n", "documents", "Policies, procedures and folders", "tasks", "risks", "assets", "audits", "management-reviews", "change-requests", "valiido more",
			"\nWORK WITH RECORDS\n", "comments", "documents upload", "<noun> import", "<noun> pdf",
			"\nACCOUNT & SYSTEM\n", "\nADVANCED\n", "\nHELP TOPICS\n", "\nFLAGS\n", "--profile NAME"}},
		{[]string{"documents", "--help"}, []string{"valiido documents <verb>", "\nCOMMANDS\n", "list", "show", "create", "update",
			"\nALIASES\n  document\n", "\nINHERITED FLAGS\n", "valiido documents list", "breadcrumbs"}},
		{[]string{"document", "read", "-h"}, []string{"Show a document", "valiido documents show ID", "--text", "--output FILE.pdf", "$ valiido documents show 42", "breadcrumbs"}},
		{[]string{"assets", "read", "--help"}, []string{"valiido assets show ID", "$ valiido assets show 42"}},
		{[]string{"assethandovers", "--help"}, []string{"valiido asset-handovers <verb>", "SUBCOMMANDS", "items"}},
		{[]string{"assethandovers", "items", "--help"}, []string{"valiido asset-handovers items <verb>", "assign", "Assign one item to an employee"}},
		{[]string{"auditmagic", "--help"}, []string{"  list-objects  ", "List records with open findings"}},
		{[]string{"auditmagic", "list_objects", "--help"}, []string{"List records with open findings", "valiido auditmagic list-objects"}},
		{[]string{"tasks", "--help"}, []string{"  show  ", "toggle-complete", "Toggle completion for one task"}},
		{[]string{"full-audits", "--help"}, []string{"  show  ", "Show a record by ID"}},
		{[]string{"tasks", "update", "--help"}, []string{"valiido tasks update", "--input JSON", "--input-file FILE.json", "--dry-run",
			"$ valiido tasks update --input-file record.json --dry-run"}},
		{[]string{"assets", "import", "--help"}, []string{"valiido assets import --file FILE.json", "$ valiido assets import --file synthetic.json --dry-run"}},
		{[]string{"comments", "list", "--help"}, []string{"--input JSON", `"resource_id":42`}},
		{[]string{"more"}, []string{"full-audits", "employee-roles", "asset-handovers", "access-rights", "boardings"}},
		{[]string{"boardings", "--help"}, []string{"SUBCOMMANDS", "steps", "checklists"}},
		{[]string{"boardings", "checklists", "--help"}, []string{"SUBCOMMANDS", "items"}},
		{[]string{"kpis", "--help"}, []string{"measurements"}},
		{[]string{"access-rights", "items", "read", "--help"}, []string{"valiido access-rights items show ID"}},
		{[]string{"help", "output"}, []string{"--json", "--quiet"}},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			var out, stderr strings.Builder
			if code := Run(t.Context(), tc.args, &out, &stderr); code != 0 || stderr.Len() != 0 {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), stderr.String())
			}
			for _, want := range tc.want {
				if !strings.Contains(out.String(), want) {
					t.Errorf("missing %q in %q", want, out.String())
				}
			}
		})
	}
}

func TestUnknownHelpCommandSuggestions(t *testing.T) {
	config := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", config)
	for _, tc := range []struct {
		args                []string
		unknown, suggestion string
	}{
		{[]string{"dokuments", "list"}, "dokuments", "documents"},
		{[]string{"risks", "lsit"}, "lsit", "list"},
		{[]string{"documetns"}, "documetns", "documents"},
		{[]string{"documents", "shwo", "--help"}, "shwo", "show"},
		{[]string{"auditmagic", "list-objekts"}, "list-objekts", "list-objects"},
		{[]string{"auth", "statsu"}, "statsu", "status"},
		{[]string{"help", "ouput"}, "ouput", "output"},
		{[]string{"boardings", "stpes", "list"}, "stpes", "steps"},
		{[]string{"boardings", "checklists", "items", "lsit"}, "lsit", "list"},
	} {
		var out, stderr strings.Builder
		if code := Run(t.Context(), tc.args, &out, &stderr); code != 2 {
			t.Fatalf("%v returned %d, %s", tc.args, code, out.String())
		}
		var result struct{ Error apiError }
		if err := json.Unmarshal([]byte(out.String()), &result); err != nil ||
			result.Error.Code != "usage" ||
			result.Error.Message != "Unknown command "+tc.unknown+"." ||
			result.Error.Hint != "Did you mean "+tc.suggestion+"?" || stderr.Len() != 0 {
			t.Errorf("%v returned %s, stderr %s", tc.args, out.String(), stderr.String())
		}
	}
	if entries, err := os.ReadDir(config); err != nil || len(entries) != 0 {
		t.Fatalf("unknown commands touched the credential directory: %v, %v", entries, err)
	}
}

func TestPublicResourcePathsAndHiddenAliases(t *testing.T) {
	areas, _, _ := strings.Cut(strings.Split(help, "\nISMS\n")[1], "\n\n")
	if n := len(strings.Split(areas, "\n")); n > 12 {
		t.Errorf("top-level help has %d ISMS entries", n)
	}
	for _, resource := range resourceNames {
		if resource.path != resource.server && strings.Contains(help, resource.server) {
			t.Errorf("top-level help exposes %s", resource.server)
		}
		if strings.Contains(resource.path, " ") && strings.Contains(help, resource.path) {
			t.Errorf("top-level help exposes nested path %s", resource.path)
		}
		for _, path := range []string{resource.path, resource.server} {
			if path == "documents" || path == "tasks" || path == "risks" {
				continue // These have dedicated handlers.
			}
			args := append(strings.Fields(path), "read", "42")
			if got := strings.Join(resourceShortcut(args), " "); got != "resource show "+resource.server+" 42" {
				t.Errorf("%v dispatches to %s", args, got)
			}
			var out strings.Builder
			var result struct{ Error apiError }
			code := Run(t.Context(), append(args, "--not-a-flag", "--json"), &out, &out)
			if err := json.Unmarshal([]byte(out.String()), &result); err != nil || code != 2 ||
				result.Error.Code != "usage" || result.Error.Message != "Unknown option --not-a-flag." {
				t.Errorf("%v did not reach the resource parser: %s", args, out.String())
			}
		}
	}
}

func TestPublicResourceCompletion(t *testing.T) {
	for group, want := range map[string]string{
		"": "management-reviews", "boardings": "steps", "boardings checklists": "items",
		"boardings checklists items": "show", "kpis": "measurements", "auditmagic": "list-objects",
	} {
		if choices := staticChoices(group); !slices.Contains(choices, want) {
			t.Errorf("%q completions %v omit %s", group, choices, want)
		}
	}
	for _, choice := range staticChoices("") {
		if strings.ContainsAny(choice, "_ ") {
			t.Errorf("top-level completion exposes %q", choice)
		}
	}
}

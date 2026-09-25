package cli

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

func TestResponseFormatsAndBreadcrumbs(t *testing.T) {
	data := map[string]any{"type": "documents", "resources": []resource{
		{ID: 12, Type: "documents", Label: "Synthetic | policy"},
		{ID: 9007199254740993, Type: "documents", Label: "Synthetic \x1b[31m record"},
	}}
	for _, tc := range []struct{ mode, contains string }{
		{"", `"summary":"2 documents"`},
		{"json", `"command":"valiido documents show 12 --profile test"`},
		{"quiet", `"resources":[`},
		{"ids-only", "12\n9007199254740993\n"},
		{"count", "2\n"},
		{"styled", "Synthetic | policy"},
		{"markdown", `| 12 | Synthetic \| policy |`},
		{"jq", "12\n"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			var got bytes.Buffer
			retries := 2
			w := &output{Writer: &got, ctx: t.Context(), profile: "test", kind: "documents", mode: tc.mode, retries: &retries}
			if tc.mode == "jq" {
				var err error
				w.jq, err = exec.LookPath("jq")
				if err != nil {
					t.Skip("jq is optional")
				}
				w.filter = ".data.resources[0].id"
			}
			if err := success(w, data); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(got.String(), tc.contains) {
				t.Fatalf("missing %q in %s", tc.contains, got.String())
			}
			if tc.mode == "json" && !strings.Contains(got.String(), `"retried":2`) {
				t.Fatal("retry count missing")
			}
			if tc.mode == "quiet" && strings.Contains(got.String(), `"ok"`) {
				t.Fatal("quiet output contains envelope")
			}
			if (tc.mode == "styled" || tc.mode == "markdown") && strings.ContainsRune(got.String(), '\x1b') {
				t.Fatal("terminal control character passed through")
			}
		})
	}
}

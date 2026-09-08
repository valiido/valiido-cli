package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/valiido/valiido-cli/skills"
)

func TestInstallSkill(t *testing.T) {
	for _, directory := range []string{".agents", ".claude"} {
		t.Run(directory, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), directory, "skills", "valiido", "SKILL.md")
			for range 2 {
				if err := installSkill(path); err != nil {
					t.Fatal(err)
				}
			}
			content, err := os.ReadFile(path)
			if err != nil || string(content) != skills.Valiido {
				t.Fatal("bundled skill was not installed")
			}
			if err := os.WriteFile(path, []byte("custom skill"), 0644); err != nil {
				t.Fatal(err)
			}
			if err := installSkill(path); err == nil {
				t.Fatal("must reject an existing different skill")
			}
			content, err = os.ReadFile(path)
			if err != nil || string(content) != "custom skill" {
				t.Fatal("existing skill was modified")
			}
		})
	}
}

func TestSetupRejectsInvalidOptions(t *testing.T) {
	for _, args := range [][]string{{"setup", "--agent", "unknown"}, {"setup", "unexpected"}} {
		var out, diagnostic bytes.Buffer
		if code := Run(context.Background(), args, &out, &diagnostic); code != 2 {
			t.Fatalf("exit code = %d, want 2", code)
		}
	}
}

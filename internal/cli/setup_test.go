package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/valiido/valiido-cli/skills"
)

func TestInstallSkill(t *testing.T) {
	for _, directory := range []string{".agents", ".claude"} {
		t.Run(directory, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), directory, "skills", "valiido", "SKILL.md")
			for range 2 {
				if err := installSkill(path, false); err != nil {
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
			reference, err := os.ReadFile(filepath.Join(filepath.Dir(path), "references", "importing.md"))
			if err != nil || string(reference) != skills.ValiidoImporting {
				t.Fatal("bundled import reference was not installed")
			}
			if err := installSkill(path, false); err == nil {
				t.Fatal("must reject an existing different skill")
			}
			content, err = os.ReadFile(path)
			if err != nil || string(content) != "custom skill" {
				t.Fatal("existing skill was modified")
			}
			if err := installSkill(path, true); err != nil {
				t.Fatal(err)
			}
			content, err = os.ReadFile(path)
			if err != nil || string(content) != skills.Valiido {
				t.Fatal("force did not install the bundled skill")
			}
		})
	}
}

func TestInstallSkillRejectsOversizedExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".agents", "skills", "valiido", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	original := bytes.Repeat([]byte("x"), len(skills.Valiido)+1)
	if err := os.WriteFile(path, original, 0644); err != nil {
		t.Fatal(err)
	}
	if err := installSkill(path, false); err == nil {
		t.Fatal("accepted an oversized existing skill without --force")
	}
	content, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(content, original) {
		t.Fatal("modified an oversized existing skill")
	}
}

func TestSetupRejectsInvalidOptions(t *testing.T) {
	for _, args := range [][]string{{"setup", "--agent", "unknown"}, {"setup", "unexpected"}, {"setup", "--agent", "codex", "--agent", "claude"}, {"setup", "--force", "--force"}} {
		var out, diagnostic bytes.Buffer
		if code := Run(context.Background(), args, &out, &diagnostic); code != 2 {
			t.Fatalf("exit code = %d, want 2", code)
		}
	}
}

func TestSetupInstallsExactSkillForCodexClaudeAndAll(t *testing.T) {
	for _, agent := range []string{"codex", "claude", "all"} {
		t.Run(agent, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			var output bytes.Buffer
			if code := Run(context.Background(), []string{"setup", "--agent", agent}, &output, &output); code != 0 {
				t.Fatalf("setup failed: %s", output.String())
			}
			var response struct {
				Data struct {
					Skills []string `json:"skills"`
				} `json:"data"`
			}
			if json.Unmarshal(output.Bytes(), &response) != nil {
				t.Fatalf("invalid setup output: %s", output.String())
			}
			want := map[string]bool{"codex": agent == "codex" || agent == "all", "claude": agent == "claude" || agent == "all"}
			if len(response.Data.Skills) != map[bool]int{true: 2, false: 1}[agent == "all"] {
				t.Fatalf("wrong installed path count: %#v", response.Data.Skills)
			}
			for name, expected := range want {
				directory := map[string]string{"codex": ".agents", "claude": ".claude"}[name]
				path := filepath.Join(home, directory, "skills", "valiido", "SKILL.md")
				content, err := os.ReadFile(path)
				if expected && (err != nil || string(content) != skills.Valiido) {
					t.Errorf("%s skill was not installed byte-for-byte", name)
				}
				reference, referenceErr := os.ReadFile(filepath.Join(filepath.Dir(path), "references", "importing.md"))
				if expected && (referenceErr != nil || string(reference) != skills.ValiidoImporting) {
					t.Errorf("%s import reference was not installed byte-for-byte", name)
				}
				if !expected && !os.IsNotExist(err) {
					t.Errorf("unexpected %s skill installation", name)
				}
			}
		})
	}
}

func TestSetupAllPreflightsConflictsBeforeWritingFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	conflict := filepath.Join(home, ".claude", "skills", "valiido", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(conflict), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(conflict, []byte("custom"), 0644); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if code := Run(context.Background(), []string{"setup"}, &output, &output); code != 8 {
		t.Fatalf("setup should fail on conflict: %s", output.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".agents", "skills", "valiido", "SKILL.md")); !os.IsNotExist(err) {
		t.Fatal("setup all partially installed the Codex skill")
	}
	content, _ := os.ReadFile(conflict)
	if string(content) != "custom" {
		t.Fatal("setup modified the conflicting skill")
	}
}

func TestSetupForceAndExistingFilePermissions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".agents", "skills", "valiido", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("custom"), 0644); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if code := Run(context.Background(), []string{"setup", "--agent", "codex", "--force"}, &output, &output); code != 0 {
		t.Fatalf("force setup failed: %s", output.String())
	}
	if err := os.Chmod(path, 0666); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if code := Run(context.Background(), []string{"setup", "--agent", "codex"}, &output, &output); code != 0 {
		t.Fatalf("idempotent setup failed: %s", output.String())
	}
	content, err := os.ReadFile(path)
	info, statErr := os.Stat(path)
	if err != nil || statErr != nil || string(content) != skills.Valiido || info.Mode().Perm() != 0644 {
		t.Fatalf("skill content or permissions are wrong: mode=%v", info.Mode())
	}
}

func TestSetupRejectsSymlinkedSkillPaths(t *testing.T) {
	home := t.TempDir()
	outside := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.Symlink(outside, filepath.Join(home, ".agents")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	var output bytes.Buffer
	if code := Run(context.Background(), []string{"setup", "--agent", "codex", "--force"}, &output, &output); code != 8 {
		t.Fatalf("accepted symlinked skill directory: %s", output.String())
	}
	if _, err := os.Stat(filepath.Join(outside, "skills", "valiido", "SKILL.md")); !os.IsNotExist(err) {
		t.Fatal("setup escaped through a directory symlink")
	}
}

func TestSetupRejectsFinalSkillSymlinkEvenWithForce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	target := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(target, []byte("outside"), 0644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".agents", "skills", "valiido", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	var output bytes.Buffer
	if code := Run(context.Background(), []string{"setup", "--agent", "codex", "--force"}, &output, &output); code != 8 {
		t.Fatalf("accepted final skill symlink: %s", output.String())
	}
	content, _ := os.ReadFile(target)
	if string(content) != "outside" {
		t.Fatal("setup overwrote a symlink target")
	}
}

func TestSetupRequiresAbsoluteHomeAndIgnoresXDGConfigHome(t *testing.T) {
	for _, home := range []string{"", "relative/home"} {
		t.Run(home, func(t *testing.T) {
			t.Setenv("HOME", home)
			var output bytes.Buffer
			if code := Run(context.Background(), []string{"setup", "--agent", "codex"}, &output, &output); code != 8 {
				t.Fatalf("accepted unsafe HOME %q: %s", home, output.String())
			}
		})
	}
	home := t.TempDir()
	xdg := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	var output bytes.Buffer
	if code := Run(context.Background(), []string{"setup", "--agent", "codex"}, &output, &output); code != 0 {
		t.Fatalf("setup failed: %s", output.String())
	}
	if _, err := os.Stat(filepath.Join(xdg, ".agents")); !os.IsNotExist(err) {
		t.Fatal("setup incorrectly installed the skill under XDG_CONFIG_HOME")
	}
}

func TestSetupReportsUnwritableSkillDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission semantics")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	agentDirectory := filepath.Join(home, ".agents")
	if err := os.Mkdir(agentDirectory, 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(agentDirectory, 0700)
	var output bytes.Buffer
	if code := Run(context.Background(), []string{"setup", "--agent", "codex"}, &output, &output); code != 8 {
		t.Fatalf("setup accepted an unwritable skill directory: %s", output.String())
	}
}

func TestSetupRejectsSkillDirectoryWritableByOtherUsers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission semantics")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	agentDirectory := filepath.Join(home, ".agents")
	if err := os.Mkdir(agentDirectory, 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(agentDirectory, 0777); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(agentDirectory, 0700)
	var output bytes.Buffer
	if code := Run(context.Background(), []string{"setup", "--agent", "codex"}, &output, &output); code != 8 {
		t.Fatalf("setup accepted a skill directory writable by other users: %s", output.String())
	}
}

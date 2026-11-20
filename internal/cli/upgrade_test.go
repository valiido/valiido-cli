package cli

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func releaseArchive(t *testing.T, binary []byte) []byte {
	t.Helper()
	var archive bytes.Buffer
	if runtime.GOOS == "windows" {
		writer := zip.NewWriter(&archive)
		file, _ := writer.Create("valiido.exe")
		file.Write(binary)
		writer.Close()
		return archive.Bytes()
	}
	compressed := gzip.NewWriter(&archive)
	writer := tar.NewWriter(compressed)
	writer.WriteHeader(&tar.Header{Name: "valiido", Mode: 0755, Size: int64(len(binary)), Typeflag: tar.TypeReg})
	writer.Write(binary)
	writer.Close()
	compressed.Close()
	return archive.Bytes()
}

// fakeReleases serves GitHub's latest redirect, an archive and its checksums.
func fakeReleases(t *testing.T, latest string, archive []byte, checksum string) *int {
	t.Helper()
	hits := 0
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest":
			hits++
			http.Redirect(w, r, "/releases/tag/v"+latest, http.StatusFound)
		case "/releases/download/v" + latest + "/checksums.txt":
			io.WriteString(w, checksum+"  "+releaseAsset()+"\n")
		case "/releases/download/v" + latest + "/" + releaseAsset():
			w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	})
	previous := releaseBase
	releaseBase = server.URL + "/releases"
	t.Cleanup(func() { releaseBase = previous })
	return &hits
}

func fakeInstall(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	executable := filepath.Join(t.TempDir(), "valiido")
	if err := os.WriteFile(executable, []byte("old-binary"), 0755); err != nil {
		t.Fatal(err)
	}
	previous := executablePath
	executablePath = func() (string, error) { return executable, nil }
	t.Cleanup(func() { executablePath = previous })
	return executable
}

func TestUpgradeReplacesBinaryAndRefreshesUnmodifiedSkills(t *testing.T) {
	executable := fakeInstall(t)
	home, _ := os.UserHomeDir()
	if err := installSkill(filepath.Join(home, ".claude", "skills", "valiido", "SKILL.md"), false); err != nil {
		t.Fatal(err)
	}
	edited := filepath.Join(home, ".agents", "skills", "valiido", "SKILL.md")
	os.MkdirAll(filepath.Dir(edited), 0755)
	os.WriteFile(edited, []byte("my own skill"), 0644)
	refreshed := []string{}
	previous := refreshSkill
	refreshSkill = func(_ context.Context, exe, agent string) error {
		refreshed = append(refreshed, agent)
		return nil
	}
	t.Cleanup(func() { refreshSkill = previous })
	archive := releaseArchive(t, []byte("new-binary"))
	sum := sha256.Sum256(archive)
	fakeReleases(t, "99.0.0", archive, hex.EncodeToString(sum[:]))

	var stdout bytes.Buffer
	if code := Run(t.Context(), []string{"upgrade", "--json"}, &stdout, io.Discard); code != 0 {
		t.Fatalf("upgrade failed: %s", stdout.String())
	}
	content, _ := os.ReadFile(executable)
	if string(content) != "new-binary" {
		t.Fatalf("binary was not replaced: %q", content)
	}
	if len(refreshed) != 1 || refreshed[0] != "claude" {
		t.Fatalf("expected only the unmodified claude skill to refresh, got %v", refreshed)
	}
	var envelope struct {
		Data struct {
			Latest  string `json:"latest"`
			Updated bool   `json:"updated"`
		} `json:"data"`
	}
	if json.Unmarshal(stdout.Bytes(), &envelope) != nil || envelope.Data.Latest != "99.0.0" || !envelope.Data.Updated {
		t.Fatalf("unexpected output: %s", stdout.String())
	}
}

func TestUpgradeRejectsChecksumMismatch(t *testing.T) {
	executable := fakeInstall(t)
	fakeReleases(t, "99.0.0", releaseArchive(t, []byte("tampered")), strings.Repeat("0", 64))
	var stdout bytes.Buffer
	if code := Run(t.Context(), []string{"upgrade", "--json"}, &stdout, io.Discard); code == 0 || !strings.Contains(stdout.String(), "checksum") {
		t.Fatalf("expected a checksum failure, got %d %s", code, stdout.String())
	}
	if content, _ := os.ReadFile(executable); string(content) != "old-binary" {
		t.Fatal("binary changed despite a checksum mismatch")
	}
}

func TestUpgradeReportsUpToDateAndCheckOnly(t *testing.T) {
	executable := fakeInstall(t)
	fakeReleases(t, version, nil, "")
	var stdout bytes.Buffer
	if code := Run(t.Context(), []string{"upgrade", "--json"}, &stdout, io.Discard); code != 0 || !strings.Contains(stdout.String(), "up to date") {
		t.Fatalf("expected up to date, got %d %s", code, stdout.String())
	}
	fakeReleases(t, "99.0.0", nil, "")
	stdout.Reset()
	if code := Run(t.Context(), []string{"upgrade", "--check", "--json"}, &stdout, io.Discard); code != 0 || !strings.Contains(stdout.String(), "99.0.0 is available") {
		t.Fatalf("expected an available version, got %d %s", code, stdout.String())
	}
	if content, _ := os.ReadFile(executable); string(content) != "old-binary" {
		t.Fatal("--check must not change the binary")
	}
}

func TestNotifyUpdateAsksGitHubOncePerDay(t *testing.T) {
	fakeInstall(t)
	hits := fakeReleases(t, "99.0.0", nil, "")
	for range 2 {
		var stderr bytes.Buffer
		NotifyUpdate(t.Context(), []string{"tasks", "list"}, &stderr)
		if !strings.Contains(stderr.String(), "valiido 99.0.0 is available") || !strings.Contains(stderr.String(), "valiido upgrade") {
			t.Fatalf("missing update notice: %q", stderr.String())
		}
	}
	if *hits != 1 {
		t.Fatalf("expected one GitHub request per day, got %d", *hits)
	}
	t.Setenv("VALIIDO_NO_UPDATE_CHECK", "1")
	var stderr bytes.Buffer
	NotifyUpdate(t.Context(), []string{"tasks", "list"}, &stderr)
	if stderr.Len() != 0 {
		t.Fatal("VALIIDO_NO_UPDATE_CHECK must silence the notice")
	}
}

func TestNewerVersion(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{{"1.0.1", "1.0.0", true}, {"1.10.0", "1.9.9", true}, {"1.0.0", "1.0.0", false}, {"0.9.0", "1.0.0", false}, {"v2.0.0", "1.9.0", true}} {
		if got := newerVersion(tc.a, tc.b); got != tc.want {
			t.Fatalf("newerVersion(%s, %s) = %v", tc.a, tc.b, got)
		}
	}
}

package cli

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	skills "github.com/valiido/valiido-cli"
)

// releaseBase is the GitHub releases page; tests point it at a local server.
var releaseBase = "https://github.com/valiido/valiido-cli/releases"

var executablePath = os.Executable

// refreshSkill reinstalls one agent's skill with the newly installed binary.
var refreshSkill = func(ctx context.Context, executable, agent string) error {
	return exec.CommandContext(ctx, executable, "setup", "--agent", agent, "--force", "--quiet").Run()
}

const (
	maxReleaseArchive   = 64 << 20
	updateCheckInterval = 24 * time.Hour
	updateCheckTimeout  = 2 * time.Second
)

var releaseTag = regexp.MustCompile(`/releases/tag/v(\d+\.\d+\.\d+)$`)

func runUpgrade(call *invocation) error {
	flags := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	check := flags.Bool("check", false, "Only report whether a newer version exists")
	dryRun := flags.Bool("dry-run", false, "Preview without applying changes")
	finishOutput := outputFlags(flags, call.output)
	if rejectDuplicateFlags(call.args, flags) != nil || flags.Parse(call.args) != nil || flags.NArg() != 0 {
		return usageError("Use " + commandUsage("upgrade") + ".")
	}
	if err := finishOutput(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(call.context, transferBudget)
	defer cancel()
	latest, err := latestVersion(ctx)
	if err != nil {
		return err
	}
	rememberLatest(latest)
	data := map[string]any{"version": version, "latest": latest, "updated": false}
	if !newerVersion(latest, version) {
		return writeResponse(call.output, response{OK: true, Summary: "valiido " + version + " is up to date", Data: data})
	}
	if *check || *dryRun {
		return writeResponse(call.output, response{OK: true, DryRun: *dryRun, Summary: "valiido " + latest + " is available", Data: data})
	}
	executable, err := executablePath()
	if err == nil {
		executable, err = filepath.EvalSymlinks(executable)
	}
	if err != nil {
		return failure("storage", "Cannot locate the installed valiido binary.")
	}
	agents := unmodifiedSkills()
	binary, err := downloadRelease(ctx, latest)
	if err != nil {
		return err
	}
	if err := replaceExecutable(executable, binary); err != nil {
		return err
	}
	refreshed := []string{}
	for _, agent := range agents {
		if refreshSkill(ctx, executable, agent) == nil {
			refreshed = append(refreshed, agent)
		}
	}
	data["updated"], data["skills"] = true, refreshed
	return writeResponse(call.output, response{OK: true, Summary: "Upgraded valiido " + version + " to " + latest, Data: data})
}

// latestVersion reads the tag GitHub redirects /releases/latest to, without the API rate limit.
func latestVersion(ctx context.Context) (string, error) {
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, releaseBase+"/latest", nil)
	if err != nil {
		return "", failure("protocol", "Could not build the release request.")
	}
	request.Header.Set("User-Agent", "valiido-cli/"+version)
	response, err := client.Do(request)
	if err != nil {
		return "", transportError(ctx, err, client.Timeout)
	}
	response.Body.Close()
	location, err := url.Parse(response.Header.Get("Location"))
	if err != nil || response.StatusCode < 300 || response.StatusCode > 399 {
		return "", failure("network", "Could not determine the latest valiido release.")
	}
	match := releaseTag.FindStringSubmatch(location.Path)
	if match == nil {
		return "", failure("network", "Could not determine the latest valiido release.")
	}
	return match[1], nil
}

func newerVersion(candidate, current string) bool {
	parse := func(value string) []int {
		parts := strings.SplitN(strings.TrimPrefix(value, "v"), ".", 3)
		numbers := make([]int, 3)
		for i, part := range parts {
			numbers[i], _ = strconv.Atoi(part)
		}
		return numbers
	}
	a, b := parse(candidate), parse(current)
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

func releaseAsset() string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf("valiido_%s_%s.zip", runtime.GOOS, runtime.GOARCH)
	}
	return fmt.Sprintf("valiido_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
}

func downloadRelease(ctx context.Context, latest string) ([]byte, error) {
	base := releaseBase + "/download/v" + latest + "/"
	asset := releaseAsset()
	sums, err := fetchRelease(ctx, base+"checksums.txt", 1<<20)
	if err != nil {
		return nil, err
	}
	expected := ""
	scanner := bufio.NewScanner(bytes.NewReader(sums))
	for scanner.Scan() {
		if fields := strings.Fields(scanner.Text()); len(fields) == 2 && fields[1] == asset {
			expected = fields[0]
		}
	}
	archive, err := fetchRelease(ctx, base+asset, maxReleaseArchive)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(archive)
	if expected == "" || hex.EncodeToString(sum[:]) != expected {
		return nil, failure("protocol", "The downloaded release does not match its checksum. Nothing was changed.")
	}
	binary, err := extractBinary(archive)
	if err != nil || len(binary) == 0 {
		return nil, failure("protocol", "The release archive does not contain the valiido binary.")
	}
	return binary, nil
}

func fetchRelease(ctx context.Context, address string, limit int64) ([]byte, error) {
	client := &http.Client{Timeout: transferRequestTimeout, CheckRedirect: func(request *http.Request, via []*http.Request) error {
		if len(via) >= 5 || request.URL.Scheme != "https" && request.URL.Hostname() != "127.0.0.1" {
			return errors.New("unsafe redirect")
		}
		return nil
	}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, failure("protocol", "Could not build the release request.")
	}
	request.Header.Set("User-Agent", "valiido-cli/"+version)
	response, err := client.Do(request)
	if err != nil {
		return nil, transportError(ctx, err, client.Timeout)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, failure("network", fmt.Sprintf("Release download failed (HTTP %d).", response.StatusCode))
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, failure("network", "Release download was incomplete or too large.")
	}
	return data, nil
}

func extractBinary(archive []byte) ([]byte, error) {
	if runtime.GOOS == "windows" {
		reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			return nil, err
		}
		for _, file := range reader.File {
			if file.Name == "valiido.exe" {
				content, err := file.Open()
				if err != nil {
					return nil, err
				}
				defer content.Close()
				return io.ReadAll(io.LimitReader(content, maxReleaseArchive))
			}
		}
		return nil, os.ErrNotExist
	}
	compressed, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	reader := tar.NewReader(compressed)
	for {
		header, err := reader.Next()
		if err != nil {
			return nil, err
		}
		if header.Name == "valiido" && header.Typeflag == tar.TypeReg {
			return io.ReadAll(io.LimitReader(reader, maxReleaseArchive))
		}
	}
}

// replaceExecutable swaps the binary atomically; Windows cannot overwrite a running
// executable, so the old file is moved aside first and removed on the next upgrade.
func replaceExecutable(executable string, binary []byte) error {
	dir := filepath.Dir(executable)
	_ = os.Remove(executable + ".old")
	temp, err := os.CreateTemp(dir, ".valiido-upgrade-*")
	if err != nil {
		return failure("storage", "Cannot write next to the installed binary. Reinstall valiido or check the directory permissions.")
	}
	_, writeErr := temp.Write(binary)
	closeErr := temp.Close()
	if writeErr != nil || closeErr != nil || os.Chmod(temp.Name(), 0755) != nil {
		os.Remove(temp.Name())
		return failure("storage", "Cannot write the new valiido binary.")
	}
	if runtime.GOOS == "windows" {
		if err := os.Rename(executable, executable+".old"); err != nil {
			os.Remove(temp.Name())
			return failure("storage", "Cannot replace the running valiido binary.")
		}
	}
	if err := os.Rename(temp.Name(), executable); err != nil {
		os.Remove(temp.Name())
		return failure("storage", "Cannot replace the valiido binary.")
	}
	return nil
}

// unmodifiedSkills lists agents whose installed skill still matches this binary's bundle,
// so an upgrade refreshes them without overwriting a skill someone edited.
func unmodifiedSkills() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	agents := []string{}
	for _, target := range []struct{ name, directory string }{{"codex", ".agents"}, {"claude", ".claude"}} {
		root := filepath.Join(home, target.directory, "skills", "valiido")
		if _, err := os.Stat(filepath.Join(root, "SKILL.md")); err != nil {
			continue
		}
		unchanged := true
		for relativePath, content := range skills.ValiidoFiles {
			installed, err := readBoundedFile(filepath.Join(root, filepath.FromSlash(relativePath)), int64(len(content)))
			if err == nil && string(installed) != content || err != nil && !errors.Is(err, os.ErrNotExist) {
				unchanged = false
			}
		}
		if unchanged {
			agents = append(agents, target.name)
		}
	}
	return agents
}

type updateCheck struct {
	CheckedAt time.Time `json:"checked_at"`
	Latest    string    `json:"latest"`
}

func updateCheckPath() (string, error) {
	root, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "valiido", "update-check.json"), nil
}

func rememberLatest(latest string) {
	path, err := updateCheckPath()
	if err != nil {
		return
	}
	data, _ := json.Marshal(updateCheck{CheckedAt: time.Now(), Latest: latest})
	if os.MkdirAll(filepath.Dir(path), 0700) == nil {
		_ = os.WriteFile(path, data, 0600)
	}
}

// NotifyUpdate prints a short notice on stderr when a newer release exists.
// GitHub is asked at most once a day; set VALIIDO_NO_UPDATE_CHECK=1 to turn it off.
func NotifyUpdate(ctx context.Context, args []string, stderr io.Writer) {
	if os.Getenv("VALIIDO_NO_UPDATE_CHECK") != "" || len(args) > 0 && (args[0] == "upgrade" || args[0] == "shell-completion" || args[0] == "_complete") {
		return
	}
	path, err := updateCheckPath()
	if err != nil {
		return
	}
	var cached updateCheck
	if data, err := readBoundedFile(path, 4096); err == nil {
		_ = json.Unmarshal(data, &cached)
	}
	if time.Since(cached.CheckedAt) > updateCheckInterval {
		checkCtx, cancel := context.WithTimeout(ctx, updateCheckTimeout)
		latest, err := latestVersion(checkCtx)
		cancel()
		if err == nil {
			cached.Latest = latest
		}
		rememberLatest(cached.Latest)
	}
	if cached.Latest != "" && newerVersion(cached.Latest, version) {
		fmt.Fprintf(stderr, "valiido %s is available (installed: %s). Run: valiido upgrade\n", cached.Latest, version)
	}
}

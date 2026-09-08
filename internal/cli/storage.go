package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/zalando/go-keyring"
)

type session struct {
	Server       string    `json:"server"`
	TenantNumber string    `json:"tenant_number"`
	Scope        string    `json:"scope,omitempty"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
}

type store struct {
	dir     string
	profile string
	file    bool
}

func openStore(profile string, file bool) (*store, func(), error) {
	if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`).MatchString(profile) {
		return nil, nil, failure("usage", "Profile must contain only letters, numbers, hyphens or underscores.")
	}
	root, err := os.UserConfigDir()
	if err != nil {
		return nil, nil, failure("storage", "Could not locate the user configuration directory.")
	}
	dir := filepath.Join(root, "valiido")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, nil, failure("storage", "Could not create the credential directory.")
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, nil, failure("storage", "Credential directory must be a real directory.")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return nil, nil, failure("storage", "Could not secure the credential directory.")
	}
	lock := filepath.Join(dir, profile+".lock")
	if err := os.Mkdir(lock, 0700); err != nil {
		return nil, nil, failure("busy", "Profile is in use. After a crashed process, remove its .lock directory from your Valiido configuration.")
	}
	s := &store{dir: dir, profile: profile, file: file}
	return s, func() { _ = os.Remove(lock) }, nil
}

func (s *store) path() string { return filepath.Join(s.dir, s.profile+".json") }

func (s *store) load() (session, error) {
	var value session
	var data []byte
	// A fallback file exists only after the user explicitly opted in at login.
	info, err := os.Lstat(s.path())
	if err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return value, failure("storage", "Credential file must be a regular file with mode 0600.")
		}
		s.file = true
		data, err = os.ReadFile(s.path())
	} else if errors.Is(err, os.ErrNotExist) {
		var raw string
		raw, err = keyring.Get("valiido-cli", s.profile)
		data = []byte(raw)
	}
	if err != nil || len(data) == 0 {
		return value, failure("auth", "No usable credentials. Run valiido auth login for this profile.")
	}
	if err := json.Unmarshal(data, &value); err != nil || value.RefreshToken == "" || !tenantPattern.MatchString(value.TenantNumber) {
		return value, failure("storage", "Invalid stored credentials; sign in again.")
	}
	return value, nil
}

func (s *store) save(value session) error {
	data, err := json.Marshal(value)
	if err != nil {
		return failure("storage", "Could not encode credentials.")
	}
	if !s.file {
		if err := keyring.Set("valiido-cli", s.profile, string(data)); err != nil {
			return failure("storage", "System keyring unavailable. For headless use, explicitly opt in with auth login --file-credentials.")
		}
		// Remove any previous fallback only after keyring storage succeeds.
		if err := os.Remove(s.path()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return failure("storage", "Saved to keyring but could not remove the old credential file.")
		}
		return nil
	}
	f, err := os.CreateTemp(s.dir, ".credentials-*")
	if err != nil {
		return failure("storage", "Could not create a credential file.")
	}
	defer os.Remove(f.Name())
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	closeErr := f.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return failure("storage", "Could not persist credentials.")
	}
	if err := os.Rename(f.Name(), s.path()); err != nil {
		return failure("storage", "Could not replace credentials.")
	}
	return nil
}

func (s *store) clear() error {
	if s.file {
		if err := os.Remove(s.path()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return failure("storage", "Could not remove local credentials.")
		}
		return nil
	}
	if err := keyring.Delete("valiido-cli", s.profile); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return failure("storage", "Could not remove credentials from keyring.")
	}
	return nil
}

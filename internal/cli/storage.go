package cli

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/zalando/go-keyring"
)

const (
	profileLockWait          = 5 * time.Second
	profileLockUntilDeadline = -1
	profileLockPoll          = 50 * time.Millisecond
	orphanLockGrace          = time.Second
	maxLockOwnerBytes        = 1024
	maxStoredCredentialBytes = 4096
)

var profilePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

type lockOwner struct {
	StartedAt time.Time `json:"started_at,omitempty"`
	PID       int       `json:"pid"`
	Nonce     string    `json:"nonce"`
}

type session struct {
	Server         string     `json:"server"`
	TenantNumber   string     `json:"tenant_number"`
	Scope          string     `json:"scope,omitempty"`
	AccessToken    string     `json:"access_token"`
	RefreshToken   string     `json:"refresh_token"`
	ExpiresAt      time.Time  `json:"expires_at"`
	RefreshAfter   *time.Time `json:"refresh_after,omitempty"`
	RefreshPending bool       `json:"refresh_pending,omitempty"`
}

type store struct {
	dir     string
	profile string
	file    bool
}

func openStore(profile string, file bool) (*store, func(), error) {
	return openStoreContext(context.Background(), profile, file, profileLockWait)
}

func profileStore(profile string, file bool) (*store, error) {
	if !profilePattern.MatchString(profile) {
		return nil, failure("usage", "Profile must contain only letters, numbers, hyphens or underscores.")
	}
	root, err := os.UserConfigDir()
	if err != nil {
		return nil, failure("storage", "Could not locate the user configuration directory.")
	}
	dir := filepath.Join(root, "valiido")
	if info, err := os.Lstat(dir); err == nil && !info.IsDir() {
		return nil, failure("storage", "Credential directory must be a real directory.")
	}
	return &store{dir: dir, profile: profile, file: file}, nil
}

func openStoreContext(ctx context.Context, profile string, file bool, wait time.Duration) (*store, func(), error) {
	s, err := profileStore(profile, file)
	if err != nil {
		return nil, nil, err
	}
	dir := s.dir
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
	owner := lockOwner{PID: os.Getpid(), Nonce: randomLockNonce(), StartedAt: time.Now().UTC()}
	deadline := time.Now().Add(wait)
	busy := failure("busy", "Profile is still in use. Wait for the other Valiido command to finish.")
	contended := false
	for {
		if ctx.Err() != nil {
			if contended && wait == profileLockUntilDeadline && errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, nil, busy
			}
			return nil, nil, failure("cancelled", "Profile lock wait cancelled.")
		}
		guard, err := acquireLockGuard(lock + ".guard")
		if err != nil {
			return nil, nil, failure("storage", "Could not secure the profile lock.")
		}
		if guard != nil {
			acquired, err := tryProfileLock(lock, owner)
			if err != nil {
				guard()
				return nil, nil, err
			}
			if acquired {
				var once sync.Once
				return s, func() {
					once.Do(func() {
						defer guard()
						releaseLock(lock, owner)
					})
				}, nil
			}
			guard()
		}
		contended = true
		if wait != profileLockUntilDeadline && !time.Now().Before(deadline) {
			return nil, nil, busy
		}
		waitFor(ctx, profileLockPoll)
	}
}

func tryProfileLock(lock string, owner lockOwner) (bool, error) {
	err := os.Mkdir(lock, 0700)
	if errors.Is(err, os.ErrExist) && canReclaimProfileLock && reclaimStaleLock(lock) {
		err = os.Mkdir(lock, 0700)
	}
	if errors.Is(err, os.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, failure("storage", "Could not acquire the profile lock.")
	}
	if err := writeLockOwner(lock, owner); err != nil {
		_ = os.Remove(filepath.Join(lock, "owner.json"))
		_ = os.Remove(lock)
		return false, failure("storage", "Could not secure the profile lock.")
	}
	return true, nil
}

func randomLockNonce() string {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	}
	return fmt.Sprintf("%x", value)
}

func writeLockOwner(lock string, owner lockOwner) error {
	data, err := json.Marshal(owner)
	if err != nil {
		return err
	}
	name := filepath.Join(lock, "owner.json.tmp")
	if err := os.WriteFile(name, data, 0600); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, filepath.Join(lock, "owner.json")); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

func readLockOwner(lock string) (lockOwner, error) {
	var owner lockOwner
	data, err := readBoundedFile(filepath.Join(lock, "owner.json"), maxLockOwnerBytes)
	if err != nil {
		return owner, err
	}
	err = json.Unmarshal(data, &owner)
	if err != nil || owner.PID <= 0 || owner.Nonce == "" {
		return lockOwner{}, errors.New("invalid lock owner")
	}
	return owner, nil
}

func reclaimStaleLock(lock string) bool {
	info, err := os.Lstat(lock)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	owner, err := readLockOwner(lock)
	if err != nil {
		return time.Since(info.ModTime()) >= orphanLockGrace && renameStaleLock(lock)
	}

	if processAlive(owner.PID) {
		return false
	}
	return renameStaleLock(lock)
}

func renameStaleLock(lock string) bool {
	stale := lock + ".stale-" + strconv.Itoa(os.Getpid()) + "-" + randomLockNonce()
	if os.Rename(lock, stale) != nil {
		return false
	}
	_ = os.Remove(filepath.Join(stale, "owner.json"))
	_ = os.Remove(filepath.Join(stale, "owner.json.tmp"))
	_ = os.Remove(stale)
	return true
}

func releaseLock(lock string, expected lockOwner) {
	owner, err := readLockOwner(lock)
	if err != nil || owner != expected {
		return
	}
	_ = os.Remove(filepath.Join(lock, "owner.json"))
	_ = os.Remove(lock)
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
		data, err = readBoundedFile(s.path(), maxStoredCredentialBytes)
	} else if errors.Is(err, os.ErrNotExist) {
		s.file = false
		var raw string
		raw, err = keyring.Get("valiido-cli", s.profile)
		if err != nil && !errors.Is(err, keyring.ErrNotFound) {
			failure := failure("auth", "System keyring is unavailable and this profile has no credential file.")
			failure.Hint = "Unlock the system keyring, or add --file-credentials to auth login for headless use."
			return value, failure
		}
		if len(raw) > maxStoredCredentialBytes {
			return value, failure("storage", "Invalid stored credentials; sign in again.")
		}
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

func readBoundedFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("file exceeds size limit")
	}
	return data, nil
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

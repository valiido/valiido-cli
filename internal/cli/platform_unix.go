//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package cli

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

const canReclaimProfileLock = true

// The guard stays at a stable path so two contenders cannot reclaim different
// generations of the directory lock. The OS drops it when its owner exits.
func acquireLockGuard(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return func() { _ = f.Close() }, nil
	}
	_ = f.Close()
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return nil, nil
	}
	return nil, err
}

func processAlive(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = process.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

func terminalSize(file *os.File) (int, bool) {
	size, err := unix.IoctlGetWinsize(int(file.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return 0, false
	}
	return int(size.Col), true
}

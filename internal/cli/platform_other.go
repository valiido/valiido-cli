//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !windows

package cli

import (
	"os"
)

// Without a kernel guard, reclaiming could rename a different contender's lock.
const canReclaimProfileLock = false

func acquireLockGuard(string) (func(), error) { return func() {}, nil }

// Platforms without a portable process-liveness probe keep the lock until its
// owner releases it. This is conservative and never risks concurrent rotation.
func processAlive(_ int) bool { return true }

func terminalSize(file *os.File) (int, bool) {
	return 0, false
}

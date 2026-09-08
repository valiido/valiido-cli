//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package cli

// Platforms without a portable process-liveness probe keep the lock until its
// owner releases it. This is conservative and never risks concurrent rotation.
func processAlive(_ int) bool { return true }

//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package main

import (
	"os/signal"
	"syscall"
)

func ignoreBrokenPipe() { signal.Ignore(syscall.SIGPIPE) }

package cli

import (
	"errors"
	"os"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const canReclaimProfileLock = true

var kernel32 = syscall.NewLazyDLL("kernel32.dll")
var lockFileEx = kernel32.NewProc("LockFileEx")
var getExitCodeProcess = kernel32.NewProc("GetExitCodeProcess")

func acquireLockGuard(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	var offset syscall.Overlapped
	const exclusiveImmediately = 3
	result, _, callErr := lockFileEx.Call(f.Fd(), exclusiveImmediately, 0, 1, 0, uintptr(unsafe.Pointer(&offset)))
	runtime.KeepAlive(&offset)
	if result != 0 {
		return func() { _ = f.Close() }, nil
	}
	_ = f.Close()
	if errors.Is(callErr, syscall.Errno(33)) { // ERROR_LOCK_VIOLATION
		return nil, nil
	}
	return nil, callErr
}

func processAlive(pid int) bool {
	const queryLimitedInformation = 0x1000
	handle, err := syscall.OpenProcess(queryLimitedInformation, false, uint32(pid))
	if err != nil {
		return !errors.Is(err, syscall.Errno(87)) // ERROR_INVALID_PARAMETER for an absent PID
	}
	defer syscall.CloseHandle(handle)
	var exitCode uint32
	result, _, _ := getExitCodeProcess.Call(uintptr(handle), uintptr(unsafe.Pointer(&exitCode)))
	runtime.KeepAlive(&exitCode)
	const stillActive = 259
	return result == 0 || exitCode == stillActive
}

func terminalSize(file *os.File) (int, bool) {
	handle := windows.Handle(file.Fd())
	var mode uint32
	if windows.GetConsoleMode(handle, &mode) != nil {
		return 0, false
	}
	var info windows.ConsoleScreenBufferInfo
	if windows.GetConsoleScreenBufferInfo(handle, &info) == nil {
		return int(info.Window.Right-info.Window.Left) + 1, true
	}
	return 0, true
}

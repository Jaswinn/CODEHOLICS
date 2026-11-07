//go:build windows

package storage

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

const (
	lockfileExclusiveLock   = 0x00000002
	lockfileFailImmediately = 0x00000001
	lockViolation           = syscall.Errno(33)
)

var (
	lockFileExProc   = syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")
	unlockFileExProc = syscall.NewLazyDLL("kernel32.dll").NewProc("UnlockFileEx")
	moveFileExProc   = syscall.NewLazyDLL("kernel32.dll").NewProc("MoveFileExW")
)

type processLock struct {
	file       *os.File
	overlapped syscall.Overlapped
}

func acquireProcessLock(path string) (*processLock, error) {
	lockPath := path + ".lock"
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}

	lock := &processLock{file: file}
	result, _, callErr := lockFileExProc.Call(
		file.Fd(),
		lockfileExclusiveLock|lockfileFailImmediately,
		0,
		uintptr(^uint32(0)),
		uintptr(^uint32(0)),
		uintptr(unsafe.Pointer(&lock.overlapped)),
	)
	if result == 0 {
		_ = file.Close()
		if errors.Is(callErr, lockViolation) {
			return nil, fmt.Errorf("%w: %s", ErrDatabaseLocked, path)
		}
		return nil, fmt.Errorf("lock database %s: %w", path, callErr)
	}
	return lock, nil
}

func (l *processLock) close() error {
	result, _, callErr := unlockFileExProc.Call(
		l.file.Fd(),
		0,
		uintptr(^uint32(0)),
		uintptr(^uint32(0)),
		uintptr(unsafe.Pointer(&l.overlapped)),
	)
	if result == 0 {
		return errors.Join(fmt.Errorf("unlock database: %w", callErr), l.file.Close())
	}
	return l.file.Close()
}

func replaceDatabaseFile(source, target string) error {
	sourcePath, err := syscall.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	targetPath, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	const replaceExistingAndWriteThrough = 0x00000001 | 0x00000008
	result, _, callErr := moveFileExProc.Call(
		uintptr(unsafe.Pointer(sourcePath)),
		uintptr(unsafe.Pointer(targetPath)),
		replaceExistingAndWriteThrough,
	)
	if result == 0 {
		return fmt.Errorf("replace database file: %w", callErr)
	}
	return nil
}

func syncDatabaseDirectory(string) error { return nil }

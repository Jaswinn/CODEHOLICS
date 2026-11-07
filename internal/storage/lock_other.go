//go:build !unix && !windows

package storage

import (
	"fmt"
	"os"
)

type processLock struct{}

func acquireProcessLock(path string) (*processLock, error) {
	return nil, fmt.Errorf("storage: inter-process locking is unsupported on this platform")
}

func (l *processLock) close() error { return nil }

func replaceDatabaseFile(source, target string) error {
	return os.Rename(source, target)
}

func syncDatabaseDirectory(string) error { return nil }

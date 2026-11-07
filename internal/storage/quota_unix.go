//go:build unix

package storage

import "syscall"

var errDiskQuota error = syscall.EDQUOT

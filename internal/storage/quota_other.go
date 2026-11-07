//go:build !unix

package storage

import "errors"

var errDiskQuota = errors.New("storage: no quota error on this platform")

//go:build darwin || linux

// SPDX-License-Identifier: AGPL-3.0-or-later

package benchmark

import (
	"errors"
	"os"
	"syscall"
)

func acquireLock(path string) (*os.File, error) {
	// O_NOFOLLOW also closes the final-component symlink race on creation.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, ErrStorage
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		_ = f.Close()
		return nil, ErrStorage
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrLocked
		}
		return nil, ErrStorage
	}
	return f, nil
}

//go:build !darwin && !linux

// SPDX-License-Identifier: AGPL-3.0-or-later

package benchmark

import "os"

// No unsafe fallback to an unlocked writer on unsupported operating systems.
func acquireLock(path string) (*os.File, error) { return nil, ErrUnsupported }

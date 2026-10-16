// SPDX-License-Identifier: AGPL-3.0-or-later
//go:build darwin

package runtime

import (
	"context"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestBenchmarkDescriptorDarwinACL(t *testing.T) {
	who, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	for _, directory := range []bool{false, true} {
		name := "target"
		if directory {
			name = "parent"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), name)
			mode := os.FileMode(0600)
			if directory {
				mode = 0700
				err = os.Mkdir(path, mode)
			} else {
				err = os.WriteFile(path, targetDocument(), mode)
			}
			if err != nil {
				t.Fatal(err)
			}
			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			fd := int(f.Fd())
			if !benchmarkDescriptorNoACL(fd) {
				t.Fatal("ACL-free descriptor rejected")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			// Grant the current user write access without changing restrictive modes.
			// No privilege or additional local account is required.
			if err := exec.CommandContext(ctx, "/bin/chmod", "+a", "user:"+who.Username+" allow write", path).Run(); err != nil {
				t.Fatal(err)
			}
			var st unix.Stat_t
			if err := unix.Fstat(fd, &st); err != nil {
				t.Fatal(err)
			}
			if st.Mode&0777 != uint16(mode) {
				t.Fatal("ACL test changed mode bits")
			}
			if benchmarkDescriptorNoACL(fd) {
				t.Fatal("extended ACL accepted")
			}
			// Replacing the pathname must not redirect inspection from the pinned FD.
			if err := os.Rename(path, path+".pinned"); err != nil {
				t.Fatal(err)
			}
			if directory {
				err = os.Mkdir(path, mode)
			} else {
				err = os.WriteFile(path, nil, mode)
			}
			if err != nil {
				t.Fatal(err)
			}
			if benchmarkDescriptorNoACL(fd) {
				t.Fatal("renamed ACL-bearing descriptor accepted")
			}
		})
	}
	if benchmarkDescriptorNoACL(-1) {
		t.Fatal("inspection failure accepted")
	}
}

package tunnelkeys

import (
	"os"
	"syscall"
)

func sameChangeTime(a, b os.FileInfo) bool {
	return a.Sys().(*syscall.Stat_t).Ctimespec == b.Sys().(*syscall.Stat_t).Ctimespec
}

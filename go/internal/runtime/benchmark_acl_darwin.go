// SPDX-License-Identifier: AGPL-3.0-or-later
//go:build darwin

package runtime

import (
	"encoding/binary"
	"unsafe"

	"golang.org/x/sys/unix"
)

// benchmarkDescriptorNoACL queries the pinned vnode, not its pathname (nor
// /dev/fd, whose synthesized metadata can hide the underlying vnode's ACL).
// Darwin mode bits and stat flags do not establish the absence of an ACL.
func benchmarkDescriptorNoACL(fd int) bool {
	// Darwin sys/attr.h: attrlist and ATTR_CMN_EXTENDED_SECURITY.
	// Do not request ATTR_CMN_RETURNED_ATTRS: that permits unavailable
	// attributes to be omitted instead of causing an inspection error.
	attrs := struct {
		Count                                 uint16
		Reserved                              uint16
		Common, Volume, Directory, File, Fork uint32
	}{Count: 5, Common: 0x00400000}
	// Space covers the maximum Darwin ACL. Errors, truncation, unexpected
	// layouts, and any nonempty security data all fail closed. This direct
	// Darwin syscall requires neither CGO nor an external command.
	var buf [8192]byte
	_, _, errno := unix.Syscall6(unix.SYS_FGETATTRLIST, uintptr(fd), uintptr(unsafe.Pointer(&attrs)), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0, 0)
	if errno != 0 {
		return false
	}
	u32 := func(i int) uint32 { return binary.LittleEndian.Uint32(buf[i : i+4]) }
	// uint32 size followed by attrreference_t (relative offset, data length).
	// No ACL is represented by an empty reference to the end of the buffer.
	return u32(0) == 12 && u32(4) == 8 && u32(8) == 0
}

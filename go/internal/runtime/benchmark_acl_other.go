// SPDX-License-Identifier: AGPL-3.0-or-later
//go:build !darwin

package runtime

// Preserve the existing non-Darwin ownership/mode policy. This is not a
// platform-independent assertion that extended ACLs are absent.
func benchmarkDescriptorNoACL(fd int) bool { return true }

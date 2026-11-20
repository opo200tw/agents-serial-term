//go:build !windows

package main

import "syscall"

// statPortID returns the identity of the node at path (stat follows a
// symlink): a re-enumerated device gets a new node, so a new inode and
// usually a new device number.
func statPortID(path string) (portID, error) {
	var s syscall.Stat_t
	if err := syscall.Stat(path, &s); err != nil {
		return portID{}, err
	}
	return portID{rdev: uint64(s.Rdev), ino: uint64(s.Ino)}, nil
}

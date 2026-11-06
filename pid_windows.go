//go:build windows

package main

// pidAlive is not implemented on Windows; publishHolder writes nothing there
// either, so a busy port fails as before.
func pidAlive(pid int) bool { return false }

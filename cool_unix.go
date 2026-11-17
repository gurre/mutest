//go:build unix

package main

import "syscall"

// lowestPriority puts this process, and every go command it starts after, at nice 19: the lowest
// priority, so anything else on the machine is scheduled first.
func lowestPriority() error {
	return syscall.Setpriority(syscall.PRIO_PROCESS, 0, 19)
}

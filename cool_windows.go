//go:build windows

package main

import "errors"

// lowestPriority refuses on Windows. mutest supports Linux and macOS, so -cool says it cannot keep
// its promise here rather than sweeping at full priority as if it had.
func lowestPriority() error {
	return errors.New("not supported on Windows")
}

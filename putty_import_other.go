//go:build !windows

package main

import "errors"

// importPuttySessions is Windows-only: PuTTY keeps its sessions in the
// registry. The stub exists so the package still compiles elsewhere.
func (a *App) importPuttySessions() (int, error) {
	return 0, errors.New("PuTTY import is only available on Windows")
}

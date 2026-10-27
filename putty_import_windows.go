//go:build windows

package main

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows/registry"

	"gravel/internal/sessionstore"
)

// importPuttySessions reads PuTTY's session list from
// HKCU\Software\SimonTatham\PuTTY\Sessions and converts each entry into a
// Gravel profile. PuTTY stores no passwords in the registry (only host, port
// and an optional private-key path), so nothing secret is copied; the login
// username is deliberately dropped because Gravel always prompts for it at
// connect time. Re-running the import updates by name instead of duplicating.
func (a *App) importPuttySessions() (int, error) {
	root, err := registry.OpenKey(registry.CURRENT_USER, `Software\SimonTatham\PuTTY\Sessions`, registry.READ)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return 0, fmt.Errorf("PuTTY sessions not found in the registry")
		}
		return 0, err
	}
	defer root.Close()

	names, err := root.ReadSubKeyNames(-1)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, name := range names {
		if name == "Default Settings" {
			continue
		}
		sk, err := registry.OpenKey(root, name, registry.READ)
		if err != nil {
			continue
		}
		host, _, err := sk.GetStringValue("HostName")
		port, _, _ := sk.GetIntegerValue("Port")
		keyFile, _, _ := sk.GetStringValue("PrivateKeyFile")
		proto, _, _ := sk.GetStringValue("Protocol")
		sk.Close()
		if err != nil || host == "" {
			continue
		}
		// Telnet/serial/console sessions have no meaning for an SSH client.
		if proto != "" && proto != "ssh" {
			continue
		}
		if port == 0 || port > 65535 {
			port = 22
		}
		// PuTTY writes the literal "none" when no key is configured.
		if keyFile == "none" {
			keyFile = ""
		}
		auth := sessionstore.AuthPassword
		if keyFile != "" {
			auth = sessionstore.AuthKey
		}
		if _, err := a.store.PutNamed(sessionstore.Profile{Name: name, Host: host, Port: int(port), Auth: auth, KeyPath: keyFile}); err != nil {
			a.log("putty import skipped %q: %v", name, err)
			continue
		}
		count++
	}
	return count, nil
}

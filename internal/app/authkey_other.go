//go:build !darwin && !linux

package app

import "errors"

// Other systems have no supported no-follow and ownership-check implementation.
func readAuthKeyFile(string) (string, error) {
	return "", errors.New("protected Tailscale auth key files are unsupported on this operating system")
}

//go:build darwin || linux

package app

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
)

const maxAuthKeyBytes = 4 << 10

// readAuthKeyFile validates the opened descriptor, not a path that could be
// replaced between a separate stat and open. Nonblocking open rejects FIFOs
// without waiting for a writer, and the bounded read also handles file growth.
func readAuthKeyFile(path string) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", fmt.Errorf("open auth key file: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("stat auth key file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("auth key file must be a regular file")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", errors.New("cannot check auth key file ownership")
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return "", errors.New("auth key file must be owned by the running user")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("auth key file must not be accessible to group or others (chmod 0600)")
	}
	if info.Size() > maxAuthKeyBytes {
		return "", fmt.Errorf("auth key file exceeds %d bytes", maxAuthKeyBytes)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxAuthKeyBytes+1))
	if err != nil {
		return "", fmt.Errorf("read auth key file: %w", err)
	}
	if len(b) > maxAuthKeyBytes {
		return "", fmt.Errorf("auth key file exceeds %d bytes", maxAuthKeyBytes)
	}
	return strings.TrimSpace(string(b)), nil
}

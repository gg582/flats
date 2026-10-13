//go:build darwin || linux

package app

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestReadAuthKeyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth-key")
	if err := os.WriteFile(path, []byte("  tskey-auth-test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readAuthKeyFile(path)
	if err != nil || got != "tskey-auth-test" {
		t.Fatalf("readAuthKeyFile() = %q, %v", got, err)
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	if got, err := readAuthKeyFile(path); err != nil || got != "tskey-auth-test" {
		t.Fatalf("owner-read-only auth key = %q, %v", got, err)
	}
}

func TestReadAuthKeyFileRejectsInvalidInputs(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	if err := os.WriteFile(good, []byte("tskey-auth-good"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	large := filepath.Join(dir, "large")
	if err := os.WriteFile(large, []byte(strings.Repeat("x", maxAuthKeyBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ name, path string }{
		{"symlink", link},
		{"directory", dir},
		{"named pipe", fifo},
		{"oversized", large},
		{"missing", filepath.Join(dir, "missing")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := readAuthKeyFile(tt.path); err == nil {
				t.Fatalf("accepted %s", tt.name)
			}
		})
	}
	max := filepath.Join(dir, "max")
	if err := os.WriteFile(max, []byte(strings.Repeat("k", maxAuthKeyBytes)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readAuthKeyFile(max); err != nil {
		t.Fatalf("rejected limit: %v", err)
	}
}

func TestReadAuthKeyFileRejectsInsecureModes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth-key")
	if err := os.WriteFile(path, []byte("tskey-auth-test"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []os.FileMode{0o640, 0o604, 0o666} {
		t.Run(mode.String(), func(t *testing.T) {
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
			if _, err := readAuthKeyFile(path); err == nil || !strings.Contains(err.Error(), "chmod 0600") {
				t.Fatalf("mode %04o: err=%v", mode, err)
			}
		})
	}
}

func TestReadAuthKeyFileRejectsDifferentOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("chown requires root")
	}
	path := filepath.Join(t.TempDir(), "auth-key")
	if err := os.WriteFile(path, []byte("tskey-auth-test"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(path, 1, -1); err != nil {
		t.Skipf("chown unavailable: %v", err)
	}
	if _, err := readAuthKeyFile(path); err == nil || !strings.Contains(err.Error(), "running user") {
		t.Fatalf("owner mismatch: err=%v", err)
	}
}

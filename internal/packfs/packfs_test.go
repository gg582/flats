package packfs

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenRegular(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "assets", "index.html"), []byte("public"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	f, info, err := OpenRegular(root, "assets/index.html")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil || string(data) != "public" || info.Size() != int64(len(data)) {
		t.Fatalf("read: %q, info: %v, error: %v", data, info, err)
	}
}

func TestOpenRegularRejectsReplacements(t *testing.T) {
	for _, kind := range []string{"file symlink", "parent symlink", "same-size regular file", "internal symlink", "same-inode symlink"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			outside := t.TempDir()
			assets := filepath.Join(dir, "assets")
			if err := os.Mkdir(assets, 0o755); err != nil {
				t.Fatal(err)
			}
			name := filepath.Join(assets, "index.html")
			if err := os.WriteFile(name, []byte("public"), 0o644); err != nil {
				t.Fatal(err)
			}
			secret := filepath.Join(outside, "index.html")
			if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
				t.Fatal(err)
			}
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			before, err := root.Lstat("assets/index.html")
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "file symlink":
				if err := os.Remove(name); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(secret, name); err != nil {
					t.Fatal(err)
				}
			case "parent symlink":
				if err := os.Rename(assets, assets+"-old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, assets); err != nil {
					t.Fatal(err)
				}
			case "same-size regular file":
				replacement := filepath.Join(dir, "replacement")
				if err := os.WriteFile(replacement, []byte("secret"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(replacement, name); err != nil {
					t.Fatal(err)
				}
			case "same-inode symlink":
				saved := filepath.Join(assets, "saved.html")
				if err := os.Rename(name, saved); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("saved.html", name); err != nil {
					t.Fatal(err)
				}
			case "internal symlink":
				replacement := filepath.Join(dir, "another.html")
				if err := os.WriteFile(replacement, []byte("secret"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(name); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("../another.html", name); err != nil {
					t.Fatal(err)
				}
			}
			f, _, err := openChecked(root, "assets/index.html", before)
			if f != nil {
				f.Close()
			}
			if err == nil {
				t.Fatal("replacement was accepted")
			}
		})
	}
}

func TestWalkDirCannotEscapeRoot(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	var seen []string
	err := WalkDir(dir, func(name string, _ os.DirEntry, root *os.Root) error {
		seen = append(seen, name)
		if name == "escape" {
			if _, _, err := OpenRegular(root, name); err == nil {
				t.Fatal("opened a symlink into an external directory")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range seen {
		if strings.Contains(name, "secret") {
			t.Fatalf("walk left root: %v", seen)
		}
	}
}

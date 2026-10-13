package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"
)

// packStats describes a packed directory.
type packStats struct {
	Files   int
	Bytes   int64 // uncompressed
	Skipped int
}

// skipEntry reports names that never belong in an upload. It matches the
// server's own skip list so local and remote counts agree.
func skipEntry(name string, dir bool) bool {
	switch {
	case name == ".git", name == "__MACOSX":
		return true
	case dir && name == "node_modules":
		return true
	case !dir && (name == ".DS_Store" || strings.HasPrefix(name, "._")):
		return true
	}
	return false
}

// packDir writes the regular files under root to an in-memory tar.gz with
// slash-separated paths relative to root. Links and special files are
// rejected, because the server refuses them and a link could pull files from
// outside the build into a public site. limit > 0 bounds the uncompressed
// size.
func packDir(root string, limit int64) ([]byte, packStats, error) {
	source, err := openUploadSource(root)
	if err != nil {
		return nil, packStats{}, err
	}
	defer source.close()
	if source.dir == nil {
		return nil, packStats{}, fmt.Errorf("%s is not a directory", root)
	}
	return packOpenedDir(source.dir, root, limit)
}

// packOpenedDir walks pinned directory handles. Every opened child must match
// the entry observed during enumeration, even if another process replaces
// files or directories with symlinks while the build is being packed.
func packOpenedDir(root *os.Root, name string, limit int64) ([]byte, packStats, error) {
	var st packStats
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	var walk func(*os.Root, string) error
	walk = func(dir *os.Root, prefix string) error {
		entries, err := fs.ReadDir(dir.FS(), ".")
		if err != nil {
			return err
		}
		for _, entry := range entries {
			rel := path.Join(prefix, entry.Name())
			if skipEntry(entry.Name(), entry.IsDir()) {
				st.Skipped++
				continue
			}
			if entry.Type()&fs.ModeSymlink != 0 {
				return fmt.Errorf("%s is a symbolic link; uploads cannot contain links (copy the target into the build output instead)", rel)
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.Mode()&fs.ModeSymlink != 0 {
				return fmt.Errorf("%s is a symbolic link; uploads cannot contain links (copy the target into the build output instead)", rel)
			}
			if entry.IsDir() {
				child, err := dir.OpenRoot(entry.Name())
				if err != nil {
					return err
				}
				opened, err := child.Stat(".")
				if err == nil && !os.SameFile(info, opened) {
					err = fmt.Errorf("%s changed while packing", rel)
				}
				if err == nil {
					err = walk(child, rel)
				}
				closeErr := child.Close()
				if err != nil {
					return err
				}
				if closeErr != nil {
					return closeErr
				}
				continue
			}
			if !entry.Type().IsRegular() || !info.Mode().IsRegular() {
				return fmt.Errorf("%s is not a regular file; remove it from the build output", rel)
			}

			f, err := dir.Open(entry.Name())
			if err != nil {
				return err
			}
			opened, err := f.Stat()
			if err == nil && (!opened.Mode().IsRegular() || !os.SameFile(info, opened)) {
				err = fmt.Errorf("%s changed while packing", rel)
			}
			if err != nil {
				_ = f.Close()
				return err
			}
			st.Files++
			st.Bytes += opened.Size()
			if limit > 0 && st.Bytes > limit {
				_ = f.Close()
				return fmt.Errorf("build is larger than the server's %d-byte upload limit (remove source maps, large media or dependencies from the build output)", limit)
			}
			hdr := &tar.Header{
				Name:     rel,
				Mode:     int64(opened.Mode().Perm()),
				Size:     opened.Size(),
				ModTime:  opened.ModTime().Truncate(time.Second),
				Typeflag: tar.TypeReg,
				Format:   tar.FormatPAX,
			}
			if err := tw.WriteHeader(hdr); err != nil {
				_ = f.Close()
				return err
			}
			n, copyErr := io.Copy(tw, f)
			closeErr := f.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
			if n != opened.Size() {
				return fmt.Errorf("%s changed while packing", rel)
			}
		}
		return nil
	}
	if err := walk(root, ""); err != nil {
		return nil, st, err
	}
	if st.Files == 0 {
		return nil, st, fmt.Errorf("%s has no files to upload", name)
	}
	if err := tw.Close(); err != nil {
		return nil, st, err
	}
	if err := gz.Close(); err != nil {
		return nil, st, err
	}
	return buf.Bytes(), st, nil
}

// gitInfo is the commit a build came from.
type gitInfo struct {
	SHA   string
	Dirty bool
}

// detectGit reads HEAD and the dirty flag of the repository containing dir.
// It returns ok=false when git is missing or dir is not in a repository with
// commits.
func detectGit(ctx context.Context, dir string) (gitInfo, bool) {
	git, err := exec.LookPath("git")
	if err != nil {
		return gitInfo{}, false
	}
	// Inside a git hook, GIT_DIR and friends point at the hook's repository
	// (often as a relative path); drop them so git finds dir's own repository.
	var env []string
	for _, kv := range os.Environ() {
		switch k, _, _ := strings.Cut(kv, "="); k {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR", "GIT_OBJECT_DIRECTORY", "GIT_PREFIX":
			continue
		}
		env = append(env, kv)
	}
	// Avoid taking the index lock: a status refresh must not race the user's
	// own git commands.
	env = append(env, "GIT_OPTIONAL_LOCKS=0")
	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, git, args...)
		cmd.Dir = dir
		cmd.Env = env
		out, err := cmd.Output()
		return string(out), err
	}
	sha, err := run("rev-parse", "HEAD")
	if err != nil {
		return gitInfo{}, false
	}
	gi := gitInfo{SHA: strings.TrimSpace(sha)}
	if out, err := run("status", "--porcelain"); err == nil {
		gi.Dirty = strings.TrimSpace(out) != ""
	}
	return gi, true
}

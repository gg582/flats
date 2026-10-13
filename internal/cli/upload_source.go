package cli

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// uploadSource pins the selected source before any bytes are read. Checking
// Lstat alone is insufficient: a workspace may replace the path with a link
// before an ordinary Open or WalkDir follows it.
type uploadSource struct {
	info fs.FileInfo
	dir  *os.Root
	file *os.File
}

func (s *uploadSource) close() {
	if s.dir != nil {
		_ = s.dir.Close()
	}
	if s.file != nil {
		_ = s.file.Close()
	}
}

func openUploadSource(name string) (*uploadSource, error) {
	clean := filepath.Clean(name) // Remove trailing separators before Lstat.
	info, err := os.Lstat(clean)
	if err != nil {
		return nil, err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is a symbolic link; uploads cannot follow links (copy the target into the build output instead)", name)
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is neither a directory nor an archive file", name)
	}

	// A volume/filesystem root has no final component to open relative to
	// its parent. Check that the opened directory is the one Lstat observed.
	if clean == filepath.Dir(clean) {
		dir, err := os.OpenRoot(clean)
		if err != nil {
			return nil, err
		}
		opened, err := dir.Stat(".")
		if err == nil && !os.SameFile(info, opened) {
			err = fmt.Errorf("%s changed before upload", name)
		}
		if err != nil {
			_ = dir.Close()
			return nil, err
		}
		return &uploadSource{info: opened, dir: dir}, nil
	}

	parent, err := os.OpenRoot(filepath.Dir(clean))
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	base := filepath.Base(clean)
	if info.IsDir() {
		dir, err := parent.OpenRoot(base)
		if err != nil {
			return nil, err
		}
		opened, err := dir.Stat(".")
		if err == nil && !os.SameFile(info, opened) {
			err = fmt.Errorf("%s changed before upload", name)
		}
		if err != nil {
			_ = dir.Close()
			return nil, err
		}
		return &uploadSource{info: opened, dir: dir}, nil
	}

	file, err := parent.Open(base)
	if err != nil {
		return nil, err
	}
	opened, err := file.Stat()
	if err == nil && (!opened.Mode().IsRegular() || !os.SameFile(info, opened)) {
		err = fmt.Errorf("%s changed before upload", name)
	}
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return &uploadSource{info: opened, file: file}, nil
}

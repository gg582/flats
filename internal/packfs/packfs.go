// Package packfs confines directory packing to its original filesystem root.
package packfs

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// WalkDir visits a tree through a single stable root. The visitor must use
// OpenRegular for file reads, rather than reopening names through the host FS.
func WalkDir(dir string, visit func(string, fs.DirEntry, *os.Root) error) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	return fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return visit(name, d, root)
	})
}

// OpenRegular opens a file without leaving root, then checks that the opened
// descriptor still refers to the regular file observed before the open.
func OpenRegular(root *os.Root, name string) (*os.File, fs.FileInfo, error) {
	before, err := root.Lstat(filepath.FromSlash(name))
	if err != nil {
		return nil, nil, err
	}
	return openChecked(root, name, before)
}

func openChecked(root *os.Root, name string, before fs.FileInfo) (*os.File, fs.FileInfo, error) {
	if !before.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%s is not a regular file", name)
	}
	f, err := root.Open(filepath.FromSlash(name))
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	current, err := root.Lstat(filepath.FromSlash(name))
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if !info.Mode().IsRegular() || !current.Mode().IsRegular() ||
		!os.SameFile(before, info) || !os.SameFile(current, info) {
		f.Close()
		return nil, nil, fmt.Errorf("%s changed while packing", name)
	}
	return f, info, nil
}

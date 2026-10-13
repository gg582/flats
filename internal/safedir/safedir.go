// Package safedir opens checked files without leaving a directory root.
package safedir

import (
	"fmt"
	"io/fs"
	"os"
)

// OpenRegular opens name beneath root and verifies that the file did not
// change between inspection and opening. Metadata belongs to the opened
// descriptor, not to the directory entry discovered during the walk.
func OpenRegular(root *os.Root, name string) (*os.File, fs.FileInfo, error) {
	before, err := root.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%s is not a regular file", name)
	}
	return openChecked(root, name, before)
}

func openChecked(root *os.Root, name string, before fs.FileInfo) (*os.File, fs.FileInfo, error) {
	f, err := root.Open(name)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if !info.Mode().IsRegular() || !os.SameFile(before, info) {
		f.Close()
		return nil, nil, fmt.Errorf("%s changed while opening", name)
	}
	return f, info, nil
}

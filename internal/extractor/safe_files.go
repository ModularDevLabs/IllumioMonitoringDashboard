package extractor

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// rootedPath separates an explicitly selected directory from the relative file
// operated on within it. os.Root enforces that the file cannot escape that
// directory through traversal or symlinks between validation and use.
type rootedPath struct {
	root     *os.Root
	name     string
	absolute string
}

func openRootedPath(rawPath string) (*rootedPath, error) {
	cleaned := filepath.Clean(strings.TrimSpace(rawPath))
	if cleaned == "." || !filepath.IsAbs(cleaned) {
		return nil, fmt.Errorf("artifact path must be absolute")
	}
	directory, name := filepath.Split(cleaned)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." || name != filepath.Base(name) {
		return nil, fmt.Errorf("artifact path must end in a filename")
	}
	// The caller may select the absolute directory, but all subsequent access is
	// performed through os.Root with a single base filename. This is the security
	// boundary that rejects traversal and symlink escape. lgtm[go/path-injection]
	root, err := os.OpenRoot(filepath.Clean(directory))
	if err != nil {
		return nil, err
	}
	return &rootedPath{root: root, name: name, absolute: cleaned}, nil
}

func (path *rootedPath) Close() error {
	if path == nil || path.root == nil {
		return nil
	}
	return path.root.Close()
}

func createExclusiveRootedFile(rawPath string, perm fs.FileMode) (*os.File, func(), error) {
	path, err := openRootedPath(rawPath)
	if err != nil {
		return nil, nil, err
	}
	file, err := path.root.OpenFile(path.name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		_ = path.Close()
		return nil, nil, err
	}
	return file, func() { _ = path.Close() }, nil
}

func openRootedFile(rawPath string) (*os.File, func(), error) {
	path, err := openRootedPath(rawPath)
	if err != nil {
		return nil, nil, err
	}
	file, err := path.root.Open(path.name)
	if err != nil {
		_ = path.Close()
		return nil, nil, err
	}
	return file, func() { _ = path.Close() }, nil
}

func readRootedFile(rawPath string) ([]byte, error) {
	path, err := openRootedPath(rawPath)
	if err != nil {
		return nil, err
	}
	defer path.Close()
	return path.root.ReadFile(path.name)
}

func statRootedFile(rawPath string) (fs.FileInfo, error) {
	path, err := openRootedPath(rawPath)
	if err != nil {
		return nil, err
	}
	defer path.Close()
	return path.root.Stat(path.name)
}

func lstatRootedFile(rawPath string) (fs.FileInfo, error) {
	path, err := openRootedPath(rawPath)
	if err != nil {
		return nil, err
	}
	defer path.Close()
	return path.root.Lstat(path.name)
}

func removeRootedFile(rawPath string) error {
	path, err := openRootedPath(rawPath)
	if err != nil {
		return err
	}
	defer path.Close()
	return path.root.Remove(path.name)
}

func openExistingRoot(rawDirectory string) (*os.Root, error) {
	directory := filepath.Clean(strings.TrimSpace(rawDirectory))
	if directory == "." || !filepath.IsAbs(directory) {
		return nil, fmt.Errorf("directory must be absolute")
	}
	// The directory is an explicit local destination; os.Root confines every
	// child operation to it. lgtm[go/path-injection]
	return os.OpenRoot(directory)
}

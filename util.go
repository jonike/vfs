package vfs

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	pathpkg "path"
	"strings"
	"time"
)

var (
	// SkipDir is used by a WalkFunc to signal Walk that
	// it wans to skip the given directory.
	SkipDir = errors.New("skip this directory")
	// ErrReadOnly is returned from Write() on a read-only file.
	ErrReadOnly = errors.New("can't write to read only file")
	// ErrWriteOnly is returned from Read() on a write-only file.
	ErrWriteOnly = errors.New("can't read from write only file")
)

type file struct {
	f        *File
	offset   int
	readable bool
	writable bool
}

func (f *file) Read(p []byte) (int, error) {
	if !f.readable {
		return 0, ErrWriteOnly
	}
	f.f.RLock()
	defer f.f.RUnlock()
	if f.offset > len(f.f.Data) {
		return 0, io.EOF
	}
	n := copy(p, f.f.Data[f.offset:])
	f.offset += n
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (f *file) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case os.SEEK_SET:
		f.offset = int(offset)
	case os.SEEK_CUR:
		f.offset += int(offset)
	case os.SEEK_END:
		f.offset = len(f.f.Data) + int(offset)
	default:
		panic(fmt.Errorf("Seek: invalid whence %d", whence))
	}
	if f.offset > len(f.f.Data) {
		f.offset = len(f.f.Data)
	} else if f.offset < 0 {
		f.offset = 0
	}
	return int64(f.offset), nil
}

func (f *file) Write(p []byte) (int, error) {
	if !f.writable {
		return 0, ErrReadOnly
	}
	f.f.Lock()
	defer f.f.Unlock()
	count := len(p)
	n := copy(f.f.Data[f.offset:], p)
	if n < count {
		f.f.Data = append(f.f.Data, p[n:]...)
	}
	f.offset += count
	f.f.ModTime = time.Now()
	return count, nil
}

func (f *file) Close() error {
	return nil
}

// WalkFunc is the function type used by Walk to iterate over a VFS.
type WalkFunc func(fs VFS, path string, info os.FileInfo, err error) error

func walk(fs VFS, p string, info os.FileInfo, fn WalkFunc) error {
	err := fn(fs, p, info, nil)
	if err != nil {
		if info.IsDir() && err == SkipDir {
			err = nil
		}
		return err
	}
	if !info.IsDir() {
		return nil
	}
	infos, err := fs.ReadDir(p)
	if err != nil {
		return fn(fs, p, info, err)
	}
	for _, v := range infos {
		name := pathpkg.Join(p, v.Name())
		fileInfo, err := fs.Lstat(name)
		if err != nil {
			if err := fn(fs, name, fileInfo, err); err != nil && err != SkipDir {
				return err
			}
			continue
		}
		if err := walk(fs, name, fileInfo, fn); err != nil && (!fileInfo.IsDir() || err != SkipDir) {
			return err
		}
	}
	return nil
}

// Walk iterates over all the files in the VFS which descend from the given
// root (including root itself), descending into any subdirectories. In each
// directory, files are visited in alphabetical order. The given function might
// chose to skip a directory by returning SkipDir.
func Walk(fs VFS, root string, fn WalkFunc) error {
	info, err := fs.Lstat(root)
	if err != nil {
		return fn(fs, root, nil, err)
	}
	return walk(fs, root, info, fn)
}

func makeDir(fs VFS, path string, perm os.FileMode) error {
	stat, err := fs.Lstat(path)
	if err == nil {
		if !stat.IsDir() {
			return fmt.Errorf("%s exists and is not a directory", path)
		}
	} else {
		if err := fs.Mkdir(path, perm); err != nil {
			return err
		}
	}
	return nil
}

// MkdirAll makes all directories pointed by the given path, using the same
// permissions for all of them. Note that MkdirAll skips directories which
// already exists rather than returning an error.
func MkdirAll(fs VFS, path string, perm os.FileMode) error {
	cur := "/"
	if err := makeDir(fs, cur, perm); err != nil {
		return err
	}
	parts := strings.Split(path, "/")
	for _, v := range parts {
		cur += v
		if err := makeDir(fs, cur, perm); err != nil {
			return err
		}
		cur += "/"
	}
	return nil
}

// RemoveAll removes all files from the given fs and path, including
// directories (by removing its contents first).
func RemoveAll(fs VFS, path string) error {
	stat, err := fs.Lstat(path)
	if err != nil {
		if err == os.ErrNotExist {
			return nil
		}
		return err
	}
	if stat.IsDir() {
		files, err := fs.ReadDir(path)
		if err != nil {
			return err
		}
		for _, v := range files {
			filePath := pathpkg.Join(path, v.Name())
			if err := RemoveAll(fs, filePath); err != nil {
				return err
			}
		}
	}
	return fs.Remove(path)
}

// ReadFile reads the file at the given path from the given fs, returning
// either its contents or an error if the file couldn't be read.
func ReadFile(fs VFS, path string) ([]byte, error) {
	f, err := fs.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ioutil.ReadAll(f)
}

// WriteFile writes a file at the given path and fs with the given data and
// permissions. If the file already exists, WriteFile truncates it before
// writing. If the file can't be created, an error will be returned.
func WriteFile(fs VFS, path string, data []byte, perm os.FileMode) error {
	f, err := fs.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Clone copies all the files from the src VFS to dst. Note that files or directories with
// all permissions set to 0 will be set to 0755 for directories and 0644 for files. If you
// need more granularity, use Walk directly to clone the file systems.
func Clone(dst VFS, src VFS) error {
	err := Walk(src, "/", func(fs VFS, path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			perm := info.Mode() & os.ModePerm
			if perm == 0 {
				perm = 0755
			}
			err := dst.Mkdir(path, info.Mode()|perm)
			if err != nil && !IsExist(err) {
				return err
			}
			return nil
		}
		data, err := ReadFile(fs, path)
		if err != nil {
			return err
		}
		perm := info.Mode() & os.ModePerm
		if perm == 0 {
			perm = 0644
		}
		if err := WriteFile(dst, path, data, info.Mode()|perm); err != nil {
			return err
		}
		return nil
	})
	return err
}

// IsExist returns wheter the error indicates that the file or directory
// already exists.
func IsExist(err error) bool {
	return os.IsExist(err)
}

// IsExist returns wheter the error indicates that the file or directory
// does not exist.
func IsNotExist(err error) bool {
	return os.IsNotExist(err)
}

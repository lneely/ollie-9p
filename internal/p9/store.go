package p9

import (
	"os"
	"path/filepath"
	"time"
)

// syntheticFileInfo implements os.FileInfo for entries with no backing file.
type syntheticFileInfo struct {
	name  string
	mode  os.FileMode
	size  int64
	isDir bool
}

func (f *syntheticFileInfo) Name() string       { return f.name }
func (f *syntheticFileInfo) Size() int64        { return f.size }
func (f *syntheticFileInfo) Mode() os.FileMode  { return f.mode }
func (f *syntheticFileInfo) ModTime() time.Time { return time.Time{} }
func (f *syntheticFileInfo) IsDir() bool        { return f.isDir }
func (f *syntheticFileInfo) Sys() any           { return nil }

// syntheticEntryImpl implements os.DirEntry for entries with no backing file.
type syntheticEntryImpl struct {
	name  string
	mode  os.FileMode
	isDir bool
}

func (e *syntheticEntryImpl) Name() string      { return e.name }
func (e *syntheticEntryImpl) IsDir() bool       { return e.isDir }
func (e *syntheticEntryImpl) Type() os.FileMode {
	if e.isDir {
		return os.ModeDir
	}
	return 0
}
func (e *syntheticEntryImpl) Info() (os.FileInfo, error) {
	return &syntheticFileInfo{name: e.name, mode: e.mode, isDir: e.isDir}, nil
}

// syntheticEntry returns a synthetic file DirEntry.
func syntheticEntry(name string, mode os.FileMode) os.DirEntry {
	return &syntheticEntryImpl{name: name, mode: mode}
}

// syntheticDirEntry returns a synthetic directory DirEntry.
func syntheticDirEntry(name string, mode os.FileMode) os.DirEntry {
	return &syntheticEntryImpl{name: name, mode: mode, isDir: true}
}

// ReadableStore is a named collection of byte blobs supporting only read operations.
type ReadableStore interface {
	Stat(name string) (os.FileInfo, error)
	List() ([]os.DirEntry, error)
	Get(name string) ([]byte, error)
}

// WritableStore supports writing blobs by name.
type WritableStore interface {
	Put(name string, data []byte) error
}

// ReadWriteStore supports reading and writing but not structural mutations.
type ReadWriteStore interface {
	ReadableStore
	WritableStore
}

// BlobStore extends ReadWriteStore with creation and deletion.
// Suitable for content-addressed or append-only backing stores.
type BlobStore interface {
	ReadWriteStore
	Delete(name string) error
	Create(name string) error
}

// Store is a fully mutable store that also supports renaming entries.
type Store interface {
	BlobStore
	Rename(oldName, newName string) error
}

// FlatDirStore implements Store backed by a directory on the local filesystem.
type FlatDirStore struct {
	dir  string
	perm os.FileMode
}

// NewFlatDirStore returns a FlatDirStore rooted at dir.
// perm is applied to newly created or written files.
func NewFlatDirStore(dir string, perm os.FileMode) *FlatDirStore {
	return &FlatDirStore{dir: dir, perm: perm}
}

func (s *FlatDirStore) Stat(name string) (os.FileInfo, error) {
	return os.Stat(filepath.Join(s.dir, name))
}

func (s *FlatDirStore) List() ([]os.DirEntry, error) {
	return os.ReadDir(s.dir)
}

func (s *FlatDirStore) Get(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(s.dir, name))
}

func (s *FlatDirStore) Put(name string, data []byte) error {
	if err := os.MkdirAll(s.dir, 0755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.dir, name), data, s.perm)
}

func (s *FlatDirStore) Delete(name string) error {
	return os.Remove(filepath.Join(s.dir, name))
}

func (s *FlatDirStore) Create(name string) error {
	if err := os.MkdirAll(s.dir, 0755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.dir, name), nil, s.perm)
}

func (s *FlatDirStore) Rename(oldName, newName string) error {
	return os.Rename(filepath.Join(s.dir, oldName), filepath.Join(s.dir, newName))
}

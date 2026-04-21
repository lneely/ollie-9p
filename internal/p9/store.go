package p9

import (
	"os"
	"time"

	"ollie/pkg/store"
)

// Re-export core store types so existing 9p code compiles unchanged.
type (
	ReadableStore  = store.ReadableStore
	WritableStore  = store.WritableStore
	ReadWriteStore = store.ReadWriteStore
	BlobStore      = store.BlobStore
	Store          = store.Store
	FlatDirStore   = store.FlatDir
)

func NewFlatDirStore(dir string, perm os.FileMode) *FlatDirStore {
	return store.NewFlatDir(dir, perm)
}

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

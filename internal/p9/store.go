package p9

import (
	"os"

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

	syntheticFileInfo = store.SyntheticFileInfo
)

func NewFlatDirStore(dir string, perm os.FileMode) *FlatDirStore {
	return store.NewFlatDir(dir, perm)
}

func syntheticEntry(name string, mode os.FileMode) os.DirEntry {
	return store.FileEntry(name, mode)
}

func syntheticDirEntry(name string, mode os.FileMode) os.DirEntry {
	return store.DirEntry(name, mode)
}

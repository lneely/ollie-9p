// Package store defines storage interfaces and a filesystem-backed
// implementation used by the 9P server.
package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// StoreEntry is an open handle to a single named blob.
type StoreEntry interface {
	Stat() (os.FileInfo, error)
	Read() ([]byte, error)
	Write(data []byte) error
	BlockingRead(ctx context.Context, base string) ([]byte, error)
}

// Store is a named collection of entries.
type Store interface {
	Stat(name string) (os.FileInfo, error)
	List() ([]os.DirEntry, error)
	Open(name string) (StoreEntry, error)
	Create(name string) error
	Delete(name string) error
	Rename(oldName, newName string) error
}

// Runnable is a running agent that can be observed and controlled.
type Runnable interface {
	RunnableID() string
	Cancel()
	Interrupt()
	AppendLog([]byte)
	LogInfo() (length int, vers uint32)
}

// RunnableStore is a Store backed by a running agent.
type RunnableStore interface {
	Store
	Runnable
}

// EntryConfig implements StoreEntry via function pointers.
type EntryConfig struct {
	StatFn         func() (os.FileInfo, error)
	ReadFn         func() ([]byte, error)
	WriteFn        func([]byte) error
	BlockingReadFn func(context.Context, string) ([]byte, error)
}

func (e *EntryConfig) Stat() (os.FileInfo, error)                              { return e.StatFn() }
func (e *EntryConfig) Read() ([]byte, error)                                   { return e.ReadFn() }
func (e *EntryConfig) Write(data []byte) error                                 { return e.WriteFn(data) }
func (e *EntryConfig) BlockingRead(ctx context.Context, base string) ([]byte, error) {
	return e.BlockingReadFn(ctx, base)
}

// storeConfig implements Store via function pointers.
type storeConfig struct {
	StatFn   func(string) (os.FileInfo, error)
	ListFn   func() ([]os.DirEntry, error)
	OpenFn   func(string) (StoreEntry, error)
	CreateFn func(string) error
	DeleteFn func(string) error
	RenameFn func(string, string) error
}

func (s *storeConfig) Stat(name string) (os.FileInfo, error)  { return s.StatFn(name) }
func (s *storeConfig) List() ([]os.DirEntry, error)           { return s.ListFn() }
func (s *storeConfig) Open(name string) (StoreEntry, error)   { return s.OpenFn(name) }
func (s *storeConfig) Create(name string) error               { return s.CreateFn(name) }
func (s *storeConfig) Delete(name string) error               { return s.DeleteFn(name) }
func (s *storeConfig) Rename(old, new string) error           { return s.RenameFn(old, new) }

// NewFlatDir returns a Store backed by a directory on the local filesystem.
func NewFlatDir(dir string, perm os.FileMode) Store {
	join := func(name string) string { return filepath.Join(dir, name) }
	ensureDir := func() error { return os.MkdirAll(dir, 0755) }
	notBlocking := func(context.Context, string) ([]byte, error) {
		return nil, fmt.Errorf("blocking read not supported")
	}

	return &storeConfig{
		StatFn: func(name string) (os.FileInfo, error) { return os.Stat(join(name)) },
		ListFn: func() ([]os.DirEntry, error) { return os.ReadDir(dir) },
		OpenFn: func(name string) (StoreEntry, error) {
			path := join(name)
			return &EntryConfig{
				StatFn:         func() (os.FileInfo, error) { return os.Stat(path) },
				ReadFn:         func() ([]byte, error) { return os.ReadFile(path) },
				WriteFn: func(data []byte) error {
					if err := ensureDir(); err != nil {
						return err
					}
					return os.WriteFile(path, data, perm)
				},
				BlockingReadFn: notBlocking,
			}, nil
		},
		CreateFn: func(name string) error {
			if err := ensureDir(); err != nil {
				return err
			}
			return os.WriteFile(join(name), nil, perm)
		},
		DeleteFn: func(name string) error { return os.Remove(join(name)) },
		RenameFn: func(old, new string) error { return os.Rename(join(old), join(new)) },
	}
}

// SyntheticFileInfo implements os.FileInfo for entries with no backing file.
type SyntheticFileInfo struct {
	Name_  string
	Mode_  os.FileMode
	Size_  int64
	IsDir_ bool
}

func (f *SyntheticFileInfo) Name() string       { return f.Name_ }
func (f *SyntheticFileInfo) Size() int64        { return f.Size_ }
func (f *SyntheticFileInfo) Mode() os.FileMode  { return f.Mode_ }
func (f *SyntheticFileInfo) ModTime() time.Time { return time.Time{} }
func (f *SyntheticFileInfo) IsDir() bool        { return f.IsDir_ }
func (f *SyntheticFileInfo) Sys() any           { return nil }

// SyntheticEntry implements os.DirEntry for entries with no backing file.
type SyntheticEntry struct {
	Name_  string
	Mode_  os.FileMode
	IsDir_ bool
}

func (e *SyntheticEntry) Name() string      { return e.Name_ }
func (e *SyntheticEntry) IsDir() bool       { return e.IsDir_ }
func (e *SyntheticEntry) Type() os.FileMode {
	if e.IsDir_ {
		return os.ModeDir
	}
	return 0
}
func (e *SyntheticEntry) Info() (os.FileInfo, error) {
	return &SyntheticFileInfo{Name_: e.Name_, Mode_: e.Mode_, IsDir_: e.IsDir_}, nil
}

// FileEntry returns a synthetic file DirEntry.
func FileEntry(name string, mode os.FileMode) os.DirEntry {
	return &SyntheticEntry{Name_: name, Mode_: mode}
}

// DirEntry returns a synthetic directory DirEntry.
func DirEntry(name string, mode os.FileMode) os.DirEntry {
	return &SyntheticEntry{Name_: name, Mode_: mode, IsDir_: true}
}

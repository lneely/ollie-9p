package store

import (
	"context"
	"fmt"
	"os"
)

// FileSpec describes a single synthetic file exposed by a RunFileStore.
type FileSpec struct {
	Name  string
	Mode  os.FileMode
	Read  func() ([]byte, error)
	Write func([]byte) error                                     // nil = read-only
	Wait  func(ctx context.Context, base string) ([]byte, error) // nil = not waitable
	Size  func() int64                                           // optional; if nil, len(Read())
}

// RunFileStore implements RunnableStore for any Runnable using a table of FileSpecs.
type RunFileStore struct {
	*storeConfig
	Runnable
	specs []FileSpec
	index map[string]int // name -> index into specs
}

// NewRunFileStore creates a RunnableStore backed by the given Runnable and file table.
func NewRunFileStore(r Runnable, specs []FileSpec) *RunFileStore {
	rs := &RunFileStore{
		Runnable: r,
		specs:    specs,
		index:    make(map[string]int, len(specs)),
	}
	for i, s := range specs {
		rs.index[s.Name] = i
	}
	notSupported := func(string) error { return fmt.Errorf("not supported") }
	rs.storeConfig = &storeConfig{
		StatFn:   rs.stat,
		ListFn:   rs.list,
		OpenFn:   rs.open,
		DeleteFn: notSupported,
		CreateFn: notSupported,
		RenameFn: func(string, string) error { return fmt.Errorf("not supported") },
	}
	return rs
}

func (rs *RunFileStore) lookup(name string) (*FileSpec, bool) {
	i, ok := rs.index[name]
	if !ok {
		return nil, false
	}
	return &rs.specs[i], true
}

func (rs *RunFileStore) stat(name string) (os.FileInfo, error) {
	spec, ok := rs.lookup(name)
	if !ok {
		return nil, fmt.Errorf("%s: not found", name)
	}
	var size int64
	if spec.Size != nil {
		size = spec.Size()
	} else if spec.Wait != nil {
		// Wait files block on read; report large size so clients read fully.
		size = 4096
	} else {
		if data, err := spec.Read(); err == nil {
			size = int64(len(data))
		}
	}
	return &SyntheticFileInfo{Name_: name, Mode_: spec.Mode, Size_: size}, nil
}

func (rs *RunFileStore) list() ([]os.DirEntry, error) {
	entries := make([]os.DirEntry, len(rs.specs))
	for i, s := range rs.specs {
		entries[i] = FileEntry(s.Name, s.Mode)
	}
	return entries, nil
}

func (rs *RunFileStore) open(name string) (StoreEntry, error) {
	spec, ok := rs.lookup(name)
	if !ok {
		return nil, fmt.Errorf("%s: not found", name)
	}
	writeFn := func(data []byte) error { return fmt.Errorf("%s: read-only", name) }
	if spec.Write != nil {
		writeFn = spec.Write
	}
	waitFn := func(context.Context, string) ([]byte, error) {
		return nil, fmt.Errorf("%s: not a wait file", name)
	}
	if spec.Wait != nil {
		waitFn = spec.Wait
	}
	return &EntryConfig{
		StatFn:         func() (os.FileInfo, error) { return rs.stat(name) },
		ReadFn:         spec.Read,
		WriteFn:        writeFn,
		BlockingReadFn: waitFn,
	}, nil
}

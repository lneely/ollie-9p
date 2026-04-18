package p9

import (
	"os"

	"ollie/pkg/paths"
)

// UtilStore is a FlatDirStore backed by the scripts/u/ directory.
type UtilStore struct {
	*FlatDirStore
}

func NewUtilStore() *UtilStore {
	return &UtilStore{FlatDirStore: NewFlatDirStore(paths.CfgDir()+"/scripts/u", 0555)}
}

func (s *UtilStore) Stat(name string) (os.FileInfo, error) {
	return s.FlatDirStore.Stat(name)
}

func (s *UtilStore) List() ([]os.DirEntry, error) {
	return s.FlatDirStore.List()
}

func (s *UtilStore) Get(name string) ([]byte, error) {
	return s.FlatDirStore.Get(name)
}

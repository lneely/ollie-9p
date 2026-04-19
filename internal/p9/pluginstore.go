package p9

import (
	"os"

	"ollie/pkg/tools/execute"
)

// ExecStore is a read-only FlatDirStore backed by the scripts/x/ directory.
// Plugins are executables invoked by the server (e.g. elevation backends), not
// by the agent or user directly.
type ExecStore struct {
	*FlatDirStore
}

func NewExecStore() *ExecStore {
	return &ExecStore{FlatDirStore: NewFlatDirStore(execute.PluginsPath(), 0555)}
}

func (s *ExecStore) Stat(name string) (os.FileInfo, error) {
	return s.FlatDirStore.Stat(name)
}

func (s *ExecStore) List() ([]os.DirEntry, error) {
	return s.FlatDirStore.List()
}

func (s *ExecStore) Get(name string) ([]byte, error) {
	return s.FlatDirStore.Get(name)
}

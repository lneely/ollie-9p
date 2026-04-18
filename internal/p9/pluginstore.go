package p9

import (
	"os"

	"ollie/pkg/tools/execute"
)

// PluginStore is a read-only FlatDirStore backed by the scripts/x/ directory.
// Plugins are executables invoked by the server (e.g. elevation backends), not
// by the agent or user directly.
type PluginStore struct {
	*FlatDirStore
}

func NewPluginStore() *PluginStore {
	return &PluginStore{FlatDirStore: NewFlatDirStore(execute.PluginsPath(), 0555)}
}

func (s *PluginStore) Stat(name string) (os.FileInfo, error) {
	return s.FlatDirStore.Stat(name)
}

func (s *PluginStore) List() ([]os.DirEntry, error) {
	return s.FlatDirStore.List()
}

func (s *PluginStore) Get(name string) ([]byte, error) {
	return s.FlatDirStore.Get(name)
}

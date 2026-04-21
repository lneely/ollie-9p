package p9

import (
	"fmt"
	"os"
	"strings"

	"ollie/pkg/config"
	"ollie/pkg/paths"
	"ollie/pkg/store"
	"ollie/pkg/tools/execute"
)

// Re-export core store types so existing 9p code compiles unchanged.
type (
	ReadableStore  = store.ReadableStore
	WritableStore  = store.WritableStore
	ReadWriteStore = store.ReadWriteStore
	BlobStore      = store.BlobStore
	Store          = store.Store
	FlatDirStore   = store.FlatDir
	SkillStore     = store.SkillStore

	syntheticFileInfo = store.SyntheticFileInfo
)

func NewFlatDirStore(dir string, perm os.FileMode) *FlatDirStore {
	return store.NewFlatDir(dir, perm)
}

func NewSkillStore() *SkillStore {
	return store.NewSkillStore()
}

func syntheticEntry(name string, mode os.FileMode) os.DirEntry {
	return store.FileEntry(name, mode)
}

func syntheticDirEntry(name string, mode os.FileMode) os.DirEntry {
	return store.DirEntry(name, mode)
}

// --- config ---

// loadAgentConfig resolves and loads the config for a named agent.
// Returns nil (not an error) if the config file does not exist;
// BuildAgentEnv handles nil configs.
func loadAgentConfig(agentsDir, name string) *config.Config {
	f, err := os.Open(agentsDir + "/" + name + ".json")
	if err != nil {
		return nil
	}
	defer f.Close()
	cfg, _ := config.Load(f)
	return cfg
}

// --- util ---

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

// --- exec ---

// ExecStore is a read-only FlatDirStore backed by the scripts/x/ directory.
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

// --- tools ---

// ToolStore is a BlobStore backed by the tools directory.
// It extends FlatDirStore with a synthetic "idx" entry that lists all tools
// with their descriptions and argument signatures.
type ToolStore struct {
	*FlatDirStore
}

func NewToolStore() *ToolStore {
	return &ToolStore{FlatDirStore: NewFlatDirStore(execute.ToolsPath(), 0755)}
}

func (s *ToolStore) Stat(name string) (os.FileInfo, error) {
	if name == "idx" {
		return &syntheticFileInfo{Name_: "idx", Mode_: 0444}, nil
	}
	return s.FlatDirStore.Stat(name)
}

func (s *ToolStore) List() ([]os.DirEntry, error) {
	entries, err := s.FlatDirStore.List()
	result := make([]os.DirEntry, 0, len(entries)+1)
	result = append(result, syntheticEntry("idx", 0444))
	result = append(result, entries...)
	return result, err
}

func (s *ToolStore) Get(name string) ([]byte, error) {
	if name == "idx" {
		return s.index()
	}
	return s.FlatDirStore.Get(name)
}

func (s *ToolStore) index() ([]byte, error) {
	entries, err := s.FlatDirStore.List()
	if err != nil {
		return nil, err
	}
	var sb strings.Builder
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := s.FlatDirStore.Get(e.Name())
		if err != nil {
			continue
		}
		var desc, args string
		for line := range strings.SplitSeq(string(data), "\n") {
			if d, ok := strings.CutPrefix(line, "# description:"); ok {
				desc = strings.TrimSpace(d)
			} else if a, ok := strings.CutPrefix(line, "# Args:"); ok {
				args = strings.TrimSpace(a)
			} else if a, ok := strings.CutPrefix(line, "# args:"); ok {
				args = strings.TrimSpace(a)
			}
			if !strings.HasPrefix(line, "#") && line != "" {
				break
			}
		}
		if desc == "" {
			continue
		}
		fmt.Fprintf(&sb, "## %s\n", e.Name())
		fmt.Fprintf(&sb, "description: %s\n", desc)
		if args != "" {
			fmt.Fprintf(&sb, "args: %s\n", args)
		}
		sb.WriteString("\n")
	}
	return []byte(sb.String()), nil
}

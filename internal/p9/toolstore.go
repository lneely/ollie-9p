package p9

import (
	"fmt"
	"os"
	"strings"

	"ollie/pkg/tools/execute"
)

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
		return &syntheticFileInfo{name: "idx", mode: 0444}, nil
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

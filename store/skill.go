package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"ollie/pkg/paths"
	"ollie/pkg/skills"
)

// SkillStoreConfig holds optional dependencies for NewSkillStore.
// Nil functions default to their os package equivalents.
type SkillStoreConfig struct {
	Dirs      []string
	ReadDir   func(string) ([]os.DirEntry, error)
	Open      func(string) (*os.File, error)
	ReadFile  func(string) ([]byte, error)
	WriteFile func(string, []byte, os.FileMode) error
	MkdirAll  func(string, os.FileMode) error
	RemoveAll func(string) error
	Rename    func(string, string) error
}

type skillState struct {
	dirs      []string
	readDir   func(string) ([]os.DirEntry, error)
	openFile  func(string) (*os.File, error)
	readFile  func(string) ([]byte, error)
	writeFile func(string, []byte, os.FileMode) error
	mkdirAll  func(string, os.FileMode) error
	removeAll func(string) error
	rename    func(string, string) error
}

func NewSkillStore() Store {
	return NewSkillStoreWith(SkillStoreConfig{})
}

func NewSkillStoreWith(cfg SkillStoreConfig) Store {
	if cfg.Dirs == nil {
		cfg.Dirs = skillDirs()
	}
	if cfg.ReadDir == nil {
		cfg.ReadDir = os.ReadDir
	}
	if cfg.Open == nil {
		cfg.Open = os.Open
	}
	if cfg.ReadFile == nil {
		cfg.ReadFile = os.ReadFile
	}
	if cfg.WriteFile == nil {
		cfg.WriteFile = os.WriteFile
	}
	if cfg.MkdirAll == nil {
		cfg.MkdirAll = os.MkdirAll
	}
	if cfg.RemoveAll == nil {
		cfg.RemoveAll = os.RemoveAll
	}
	if cfg.Rename == nil {
		cfg.Rename = os.Rename
	}

	ss := &skillState{
		dirs:      cfg.Dirs,
		readDir:   cfg.ReadDir,
		openFile: cfg.Open,
		readFile:  cfg.ReadFile,
		writeFile: cfg.WriteFile,
		mkdirAll:  cfg.MkdirAll,
		removeAll: cfg.RemoveAll,
		rename:    cfg.Rename,
	}

	return &storeConfig{
		StatFn:   ss.stat,
		ListFn:   ss.list,
		OpenFn:   ss.open,
		DeleteFn: ss.del,
		CreateFn: ss.create,
		RenameFn: ss.ren,
	}
}

func skillDirs() []string {
	if env := os.Getenv("OLLIE_SKILLS_PATH"); env != "" {
		return strings.Split(env, ":")
	}
	return []string{paths.CfgDir() + "/skills"}
}

func (ss *skillState) listSkills() []skills.Meta {
	seen := make(map[string]bool)
	var result []skills.Meta
	for _, dir := range ss.dirs {
		entries, err := ss.readDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || seen[e.Name()] {
				continue
			}
			skillDir := filepath.Join(dir, e.Name())
			f, err := ss.openFile(filepath.Join(skillDir, "SKILL.md"))
			if err != nil {
				continue
			}
			meta, err := skills.ParseFrontMatter(f, filepath.Base(skillDir), skillDir)
			f.Close()
			if err != nil {
				continue
			}
			seen[meta.Name] = true
			result = append(result, *meta)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func (ss *skillState) readSkill(name string) ([]byte, error) {
	for _, m := range ss.listSkills() {
		if m.Name == name {
			return ss.readFile(filepath.Join(m.Dir, "SKILL.md"))
		}
	}
	return nil, os.ErrNotExist
}

func (ss *skillState) stat(name string) (os.FileInfo, error) {
	if name == "idx" {
		return &SyntheticFileInfo{Name_: "idx", Mode_: 0444}, nil
	}
	skillName := strings.TrimSuffix(name, ".md")
	if _, err := ss.readSkill(skillName); err != nil {
		return nil, fmt.Errorf("%s: not found", name)
	}
	return &SyntheticFileInfo{Name_: name, Mode_: 0666}, nil
}

func (ss *skillState) list() ([]os.DirEntry, error) {
	result := []os.DirEntry{FileEntry("idx", 0444)}
	for _, m := range ss.listSkills() {
		result = append(result, FileEntry(m.Name+".md", 0666))
	}
	return result, nil
}

func (ss *skillState) open(name string) (StoreEntry, error) {
	notBlocking := func(context.Context, string) ([]byte, error) {
		return nil, fmt.Errorf("blocking read not supported")
	}
	if name == "idx" {
		return &EntryConfig{
			StatFn:         func() (os.FileInfo, error) { return &SyntheticFileInfo{Name_: "idx", Mode_: 0444}, nil },
			ReadFn:         func() ([]byte, error) { return ss.index() },
			WriteFn:        func([]byte) error { return fmt.Errorf("idx: read-only") },
			BlockingReadFn: notBlocking,
		}, nil
	}
	skillName := strings.TrimSuffix(name, ".md")
	return &EntryConfig{
		StatFn: func() (os.FileInfo, error) {
			if _, err := ss.readSkill(skillName); err != nil {
				return nil, err
			}
			return &SyntheticFileInfo{Name_: name, Mode_: 0666}, nil
		},
		ReadFn: func() ([]byte, error) { return ss.readSkill(skillName) },
		WriteFn: func(data []byte) error {
			dir := ""
			for _, m := range ss.listSkills() {
				if m.Name == skillName {
					dir = m.Dir
					break
				}
			}
			if dir == "" {
				dir = filepath.Join(ss.dirs[0], skillName)
			}
			if err := ss.mkdirAll(dir, 0755); err != nil {
				return err
			}
			return ss.writeFile(filepath.Join(dir, "SKILL.md"), data, 0644)
		},
		BlockingReadFn: notBlocking,
	}, nil
}

func (ss *skillState) del(name string) error {
	skillName := strings.TrimSuffix(name, ".md")
	for _, m := range ss.listSkills() {
		if m.Name == skillName {
			return ss.removeAll(m.Dir)
		}
	}
	return fmt.Errorf("skill not found: %s", skillName)
}

func (ss *skillState) ren(oldName, newName string) error {
	oldSkill := strings.TrimSuffix(oldName, ".md")
	newSkill := strings.TrimSuffix(newName, ".md")
	for _, m := range ss.listSkills() {
		if m.Name == oldSkill {
			newDir := filepath.Join(filepath.Dir(m.Dir), newSkill)
			return ss.rename(m.Dir, newDir)
		}
	}
	return fmt.Errorf("skill not found: %s", oldSkill)
}

func (ss *skillState) create(name string) error {
	skillName := strings.TrimSuffix(name, ".md")
	dir := filepath.Join(ss.dirs[0], skillName)
	if err := ss.mkdirAll(dir, 0755); err != nil {
		return err
	}
	return ss.writeFile(filepath.Join(dir, "SKILL.md"), nil, 0644)
}

func (ss *skillState) index() ([]byte, error) {
	var sb strings.Builder
	for _, m := range ss.listSkills() {
		fmt.Fprintf(&sb, "## %s\n", m.Name)
		fmt.Fprintf(&sb, "description: %s\n", m.Description)
		sb.WriteString("\n")
	}
	return []byte(sb.String()), nil
}

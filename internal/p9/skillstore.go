package p9

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"ollie/pkg/paths"
	"ollie/pkg/skills"
)

// SkillStore is a BlobStore backed by the skills package.
// Skills are stored as directories containing a SKILL.md file, but are
// exposed over 9P as flat "{name}.md" entries. The synthetic "idx" entry
// lists all skills with their descriptions.
type SkillStore struct{}

func NewSkillStore() *SkillStore {
	return &SkillStore{}
}

// skillDirs returns the skill directories from OLLIE_SKILLS_PATH,
// falling back to the default skills directory.
func skillDirs() []string {
	if env := os.Getenv("OLLIE_SKILLS_PATH"); env != "" {
		return strings.Split(env, ":")
	}
	return []string{paths.CfgDir() + "/skills"}
}

// listSkills scans all skill directories and returns deduplicated metadata.
// First occurrence by directory order wins on name collision.
func listSkills() []skills.Meta {
	seen := make(map[string]bool)
	var result []skills.Meta
	for _, dir := range skillDirs() {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || seen[e.Name()] {
				continue
			}
			skillDir := filepath.Join(dir, e.Name())
			f, err := os.Open(filepath.Join(skillDir, "SKILL.md"))
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

// readSkill returns the SKILL.md content for the named skill.
func readSkill(name string) ([]byte, error) {
	for _, m := range listSkills() {
		if m.Name == name {
			return os.ReadFile(filepath.Join(m.Dir, "SKILL.md"))
		}
	}
	return nil, os.ErrNotExist
}

func (s *SkillStore) Stat(name string) (os.FileInfo, error) {
	if name == "idx" {
		return &syntheticFileInfo{name: "idx", mode: 0444}, nil
	}
	skillName := strings.TrimSuffix(name, ".md")
	if _, err := readSkill(skillName); err != nil {
		return nil, fmt.Errorf("%s: not found", name)
	}
	return &syntheticFileInfo{name: name, mode: 0666}, nil
}

func (s *SkillStore) List() ([]os.DirEntry, error) {
	result := []os.DirEntry{syntheticEntry("idx", 0444)}
	for _, m := range listSkills() {
		result = append(result, syntheticEntry(m.Name+".md", 0666))
	}
	return result, nil
}

func (s *SkillStore) Get(name string) ([]byte, error) {
	if name == "idx" {
		return s.index()
	}
	skillName := strings.TrimSuffix(name, ".md")
	return readSkill(skillName)
}

func (s *SkillStore) Put(name string, data []byte) error {
	skillName := strings.TrimSuffix(name, ".md")
	dir := ""
	for _, m := range listSkills() {
		if m.Name == skillName {
			dir = m.Dir
			break
		}
	}
	if dir == "" {
		dir = filepath.Join(skillDirs()[0], skillName)
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "SKILL.md"), data, 0644)
}

func (s *SkillStore) Delete(name string) error {
	skillName := strings.TrimSuffix(name, ".md")
	for _, m := range listSkills() {
		if m.Name == skillName {
			return os.RemoveAll(m.Dir)
		}
	}
	return fmt.Errorf("skill not found: %s", skillName)
}

func (s *SkillStore) Rename(oldName, newName string) error {
	oldSkill := strings.TrimSuffix(oldName, ".md")
	newSkill := strings.TrimSuffix(newName, ".md")
	for _, m := range listSkills() {
		if m.Name == oldSkill {
			newDir := filepath.Join(filepath.Dir(m.Dir), newSkill)
			return os.Rename(m.Dir, newDir)
		}
	}
	return fmt.Errorf("skill not found: %s", oldSkill)
}

func (s *SkillStore) Create(name string) error {
	skillName := strings.TrimSuffix(name, ".md")
	dir := filepath.Join(skillDirs()[0], skillName)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "SKILL.md"), nil, 0644)
}

func (s *SkillStore) index() ([]byte, error) {
	var sb strings.Builder
	for _, m := range listSkills() {
		fmt.Fprintf(&sb, "## %s\n", m.Name)
		fmt.Fprintf(&sb, "description: %s\n", m.Description)
		sb.WriteString("\n")
	}
	return []byte(sb.String()), nil
}

package p9

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

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

func (s *SkillStore) Stat(name string) (os.FileInfo, error) {
	if name == "idx" {
		return &syntheticFileInfo{name: "idx", mode: 0444}, nil
	}
	skillName := strings.TrimSuffix(name, ".md")
	if _, err := skills.Read(skillName); err != nil {
		return nil, fmt.Errorf("%s: not found", name)
	}
	return &syntheticFileInfo{name: name, mode: 0666}, nil
}

func (s *SkillStore) List() ([]os.DirEntry, error) {
	result := []os.DirEntry{syntheticEntry("idx", 0444)}
	for _, m := range skills.List() {
		result = append(result, syntheticEntry(m.Name+".md", 0666))
	}
	return result, nil
}

func (s *SkillStore) Get(name string) ([]byte, error) {
	if name == "idx" {
		return s.index()
	}
	skillName := strings.TrimSuffix(name, ".md")
	return skills.Read(skillName)
}

func (s *SkillStore) Put(name string, data []byte) error {
	skillName := strings.TrimSuffix(name, ".md")
	dir := ""
	for _, m := range skills.List() {
		if m.Name == skillName {
			dir = m.Dir
			break
		}
	}
	if dir == "" {
		dir = filepath.Join(skills.Dirs()[0], skillName)
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "SKILL.md"), data, 0644)
}

func (s *SkillStore) Delete(name string) error {
	skillName := strings.TrimSuffix(name, ".md")
	for _, m := range skills.List() {
		if m.Name == skillName {
			return os.RemoveAll(m.Dir)
		}
	}
	return fmt.Errorf("skill not found: %s", skillName)
}

func (s *SkillStore) Rename(oldName, newName string) error {
	oldSkill := strings.TrimSuffix(oldName, ".md")
	newSkill := strings.TrimSuffix(newName, ".md")
	for _, m := range skills.List() {
		if m.Name == oldSkill {
			newDir := filepath.Join(filepath.Dir(m.Dir), newSkill)
			return os.Rename(m.Dir, newDir)
		}
	}
	return fmt.Errorf("skill not found: %s", oldSkill)
}

func (s *SkillStore) Create(name string) error {
	skillName := strings.TrimSuffix(name, ".md")
	dir := filepath.Join(skills.Dirs()[0], skillName)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "SKILL.md"), nil, 0644)
}

func (s *SkillStore) index() ([]byte, error) {
	var sb strings.Builder
	for _, m := range skills.List() {
		fmt.Fprintf(&sb, "## %s\n", m.Name)
		fmt.Fprintf(&sb, "description: %s\n", m.Description)
		sb.WriteString("\n")
	}
	return []byte(sb.String()), nil
}

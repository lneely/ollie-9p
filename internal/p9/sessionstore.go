package p9

import (
	"fmt"
	"os"
	"strings"

	"ollie/pkg/paths"
)

// sessionStoreFiles maps fixed file entries in s/ to their permissions.
// Add an entry here to expose a new file; the server uses this map for
// routing, mode, and stat length.
var sessionStoreFiles = map[string]os.FileMode{
	"new":  0666,
	"idx":  0444,
	"ls":   0555,
	"kill": 0555,
	"sh":   0555,
}

// sessionStoreOrder defines the listing order for fixed s/ entries.
var sessionStoreOrder = []string{"new", "idx", "ls", "kill", "sh"}

// SessionStore implements Store for the /s/ directory.
// Entries are session IDs (directories) plus the synthetic files "new" and "idx".
// Put("new", data) creates a session; Delete(id) kills one; Rename renames one.
type SessionStore struct {
	srv *Server
}

func (s *SessionStore) List() ([]os.DirEntry, error) {
	entries := make([]os.DirEntry, 0, len(sessionStoreOrder))
	for _, name := range sessionStoreOrder {
		entries = append(entries, syntheticEntry(name, sessionStoreFiles[name]))
	}
	s.srv.mu.RLock()
	for id := range s.srv.sessions {
		entries = append(entries, syntheticDirEntry(id, 0555))
	}
	s.srv.mu.RUnlock()
	return entries, nil
}

func (s *SessionStore) Stat(name string) (os.FileInfo, error) {
	if mode, ok := sessionStoreFiles[name]; ok {
		return &syntheticFileInfo{Name_: name, Mode_: mode}, nil
	}
	s.srv.mu.RLock()
	_, ok := s.srv.sessions[name]
	s.srv.mu.RUnlock()
	if ok {
		return &syntheticFileInfo{Name_: name, Mode_: 0555, IsDir_: true}, nil
	}
	return nil, fmt.Errorf("%s: not found", name)
}

func (s *SessionStore) Get(name string) ([]byte, error) {
	switch name {
	case "new":
		return []byte("name=\ncwd=\nbackend=\nmodel=\nagent=\n"), nil
	case "idx":
		return s.index(), nil
	default:
		if _, ok := sessionStoreFiles[name]; ok {
			return os.ReadFile(paths.CfgDir() + "/scripts/s/" + name)
		}
	}
	return nil, fmt.Errorf("%s: not a readable file", name)
}

func (s *SessionStore) Put(name string, data []byte) error {
	if name != "new" {
		return fmt.Errorf("%s: not writable", name)
	}
	return s.srv.handleNewSession(strings.TrimSpace(string(data)))
}

func (s *SessionStore) Delete(name string) error {
	s.srv.mu.RLock()
	_, ok := s.srv.sessions[name]
	s.srv.mu.RUnlock()
	if !ok {
		return fmt.Errorf("session not found: %s", name)
	}
	s.srv.killSession(name)
	return nil
}

func (s *SessionStore) Create(name string) error {
	return fmt.Errorf("create not supported for sessions")
}

func (s *SessionStore) Rename(oldName, newName string) error {
	return s.srv.renameSession(oldName, newName)
}

func (s *SessionStore) index() []byte {
	var sb strings.Builder
	s.srv.mu.RLock()
	for id, sess := range s.srv.sessions {
		sess.mu.RLock()
		state := sess.core.State()
		cwd := sess.core.CWD()
		be := sess.core.BackendName()
		model := sess.core.ModelName()
		sess.mu.RUnlock()
		fmt.Fprintf(&sb, "%s\t%s\t%s\t%s\t%s\n", id, state, cwd, be, model)
	}
	s.srv.mu.RUnlock()
	return []byte(sb.String())
}

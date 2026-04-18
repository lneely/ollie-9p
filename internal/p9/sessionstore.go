package p9

import (
	"fmt"
	"os"
	"strings"
)

// SessionStore implements Store for the /s/ directory.
// Entries are session IDs (directories) plus the synthetic files "new" and "idx".
// Put("new", data) creates a session; Delete(id) kills one; Rename renames one.
type SessionStore struct {
	srv *Server
}

func (s *SessionStore) List() ([]os.DirEntry, error) {
	entries := []os.DirEntry{
		syntheticEntry("new", 0666),
		syntheticEntry("idx", 0444),
		syntheticEntry("ls", 0555),
	}
	s.srv.mu.RLock()
	for id := range s.srv.sessions {
		entries = append(entries, syntheticDirEntry(id, 0555))
	}
	s.srv.mu.RUnlock()
	return entries, nil
}

func (s *SessionStore) Stat(name string) (os.FileInfo, error) {
	switch name {
	case "new":
		return &syntheticFileInfo{name: "new", mode: 0666}, nil
	case "idx":
		return &syntheticFileInfo{name: "idx", mode: 0444}, nil
	case "ls":
		return &syntheticFileInfo{name: "ls", mode: 0555}, nil
	}
	s.srv.mu.RLock()
	_, ok := s.srv.sessions[name]
	s.srv.mu.RUnlock()
	if ok {
		return &syntheticFileInfo{name: name, mode: 0555, isDir: true}, nil
	}
	return nil, fmt.Errorf("%s: not found", name)
}

func (s *SessionStore) Get(name string) ([]byte, error) {
	switch name {
	case "new":
		return []byte("name=\ncwd=\nbackend=\nmodel=\nagent=\n"), nil
	case "idx":
		return s.index(), nil
	case "ls":
		return []byte("#!/usr/bin/env bash\ncat ${OLLIE:-$HOME/mnt/ollie}/s/idx\n"), nil
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

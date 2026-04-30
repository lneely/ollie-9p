package store

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"ollie/pkg/agent"
	"ollie/pkg/backend"
	"ollie/pkg/config"
	olog "ollie/pkg/log"
	"ollie/pkg/paths"
	"ollie/pkg/tools"
	"ollie/pkg/tools/execute"
)

// Session holds all state for one agent session.
type Session struct {
	mu         sync.RWMutex
	id         string
	Core       agent.Core
	Ctx        context.Context
	cancel     context.CancelFunc
	log        []byte
	logVers    uint32
	ChatOffset int
	plan       []byte
	prevPrompt []byte // last submitted prompt; overwritten on each new submission
}

func NewSession(id string, core agent.Core, ctx context.Context, cancel context.CancelFunc) *Session {
	return &Session{id: id, Core: core, Ctx: ctx, cancel: cancel}
}

func (sess *Session) RunnableID() string { return sess.id }

func (sess *Session) Cancel() {
	sess.cancel()
}

func (sess *Session) Interrupt() {
	sess.Core.Interrupt(agent.ErrInterrupted)
}

// AppendLog appends data to the session's log and bumps the version.
func (sess *Session) AppendLog(data []byte) {
	if len(data) == 0 {
		return
	}
	sess.mu.Lock()
	sess.log = append(sess.log, data...)
	sess.logVers++
	sess.mu.Unlock()
}

// EnsureTrailingNewline appends a newline if the log doesn't already end with one.
func (sess *Session) EnsureTrailingNewline() {
	sess.mu.Lock()
	if len(sess.log) > 0 && sess.log[len(sess.log)-1] != '\n' {
		sess.log = append(sess.log, '\n')
		sess.logVers++
	}
	sess.mu.Unlock()
}

// LogInfo returns the current log length and version atomically.
func (sess *Session) LogInfo() (length int, vers uint32) {
	sess.mu.RLock()
	defer sess.mu.RUnlock()
	return len(sess.log), sess.logVers
}

// sessionStoreFiles maps fixed file entries in s/ to their permissions.
var sessionStoreFiles = map[string]os.FileMode{
	"new":     0666,
	"idx":     0444,
	"ls":      0555,
	"kill":    0555,
	"sh":      0555,
	"b":       0555,
	"bfg":     0555,
	"bbg":     0555,
	"cleanup": 0555,
}

var sessionStoreOrder = []string{"new", "idx", "ls", "kill", "sh", "b", "bfg", "bbg", "cleanup"}

// SessionStoreFileMode returns the mode for a fixed session store file,
// or 0 and false if the name is not a fixed file.
func SessionStoreFileMode(name string) (os.FileMode, bool) {
	m, ok := sessionStoreFiles[name]
	return m, ok
}

// SessionStoreConfig holds the dependencies for a SessionStore.
type SessionStoreConfig struct {
	AgentsDir   string
	SessionsDir string
	Log         *olog.Logger
	Sink        *olog.Sink
	ReadFile    func(string) ([]byte, error)
	MkdirAll    func(string, os.FileMode) error
	// OnRename is called after a session is renamed in the map,
	// for protocol-level fixups (e.g. fid path rewriting).
	OnRename       func(oldID, newID string)
	SaveTranscript func([]byte) error
	// NewCore, if non-nil, replaces the default backend.New + agent.NewAgentCore
	// path. It receives the session ID, agent name, and cwd, and returns a Core.
	NewCore func(sessionID, agentName, cwd string) (agent.Core, error)
}

// SessionStore implements Store for session management.
type SessionStore struct {
	*storeConfig
	cfg      SessionStoreConfig
	mu       sync.RWMutex
	sessions map[string]*Session
}

func NewSessionStore(cfg SessionStoreConfig) *SessionStore {
	if cfg.ReadFile == nil {
		cfg.ReadFile = os.ReadFile
	}
	if cfg.MkdirAll == nil {
		cfg.MkdirAll = os.MkdirAll
	}
	ss := &SessionStore{
		cfg:      cfg,
		sessions: make(map[string]*Session),
	}
	ss.storeConfig = &storeConfig{
		StatFn:   ss.stat,
		ListFn:   ss.list,
		OpenFn:   ss.openEntry,
		DeleteFn: ss.del,
		CreateFn: func(string) error { return fmt.Errorf("create not supported for sessions") },
		RenameFn: ss.renameSession,
	}
	return ss
}

// AddSession inserts a pre-built session into the store.
func (s *SessionStore) AddSession(sess *Session) {
	s.mu.Lock()
	s.sessions[sess.RunnableID()] = sess
	s.mu.Unlock()
}

func (s *SessionStore) list() ([]os.DirEntry, error) {
	entries := make([]os.DirEntry, 0, len(sessionStoreOrder))
	for _, name := range sessionStoreOrder {
		entries = append(entries, FileEntry(name, sessionStoreFiles[name]))
	}
	s.mu.RLock()
	for id := range s.sessions {
		entries = append(entries, DirEntry(id, 0555))
	}
	s.mu.RUnlock()
	return entries, nil
}

func (s *SessionStore) stat(name string) (os.FileInfo, error) {
	if mode, ok := sessionStoreFiles[name]; ok {
		return &SyntheticFileInfo{Name_: name, Mode_: mode}, nil
	}
	s.mu.RLock()
	_, ok := s.sessions[name]
	s.mu.RUnlock()
	if ok {
		return &SyntheticFileInfo{Name_: name, Mode_: 0555, IsDir_: true}, nil
	}
	return nil, fmt.Errorf("%s: not found", name)
}

func (s *SessionStore) openEntry(name string) (StoreEntry, error) {
	notBlocking := func(context.Context, string) ([]byte, error) {
		return nil, fmt.Errorf("blocking read not supported")
	}
	switch name {
	case "new":
		return &EntryConfig{
			StatFn: func() (os.FileInfo, error) { return &SyntheticFileInfo{Name_: "new", Mode_: 0666}, nil },
			ReadFn: func() ([]byte, error) { return []byte("name=\ncwd=\nbackend=\nmodel=\nagent=\n"), nil },
			WriteFn: func(data []byte) error {
				return s.createSession(strings.Fields(strings.TrimSpace(string(data))))
			},
			BlockingReadFn: notBlocking,
		}, nil
	case "idx":
		return &EntryConfig{
			StatFn:         func() (os.FileInfo, error) { return &SyntheticFileInfo{Name_: "idx", Mode_: 0444}, nil },
			ReadFn:         func() ([]byte, error) { return s.index(), nil },
			WriteFn:        func([]byte) error { return fmt.Errorf("idx: read-only") },
			BlockingReadFn: notBlocking,
		}, nil
	default:
		if _, ok := sessionStoreFiles[name]; ok {
			return &EntryConfig{
				StatFn: func() (os.FileInfo, error) { return &SyntheticFileInfo{Name_: name, Mode_: sessionStoreFiles[name]}, nil },
				ReadFn: func() ([]byte, error) {
					return s.cfg.ReadFile(paths.CfgDir() + "/scripts/s/" + name)
				},
				WriteFn:        func([]byte) error { return fmt.Errorf("%s: not writable", name) },
				BlockingReadFn: notBlocking,
			}, nil
		}
	}
	return nil, fmt.Errorf("%s: not found", name)
}

func (s *SessionStore) del(name string) error {
	s.mu.RLock()
	_, ok := s.sessions[name]
	s.mu.RUnlock()
	if ok {
		s.KillSession(name)
		return nil
	}
	return fmt.Errorf("session not found: %s", name)
}

// Session returns the session for the given ID, or nil.
func (s *SessionStore) Session(id string) *Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sessions[id]
}

// OpenStore returns a RunnableStore for the given session ID.
func (s *SessionStore) OpenStore(id string) (RunnableStore, error) {
	if sess := s.Session(id); sess != nil {
		return NewSessionFileStore(
			sess,
			s.cfg.Log,
			func() { s.KillSession(id) },
			func(newID string) error { return s.Rename(id, newID) },
			s.cfg.SaveTranscript,
		), nil
	}
	return nil, fmt.Errorf("session not found: %s", id)
}

// InterruptAll interrupts every active session.
func (s *SessionStore) InterruptAll() {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, sess := range s.sessions {
		sess.Core.Interrupt(agent.ErrInterrupted)
	}
}

// Shutdown kills all active sessions.
func (s *SessionStore) Shutdown() {
	s.mu.Lock()
	ids := make([]string, 0, len(s.sessions))
	for id := range s.sessions {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.KillSession(id)
	}
}

func (s *SessionStore) KillSession(id string) {
	s.mu.Lock()
	sess := s.sessions[id]
	delete(s.sessions, id)
	s.mu.Unlock()
	if sess != nil {
		sess.Cancel()
		sess.Core.Close()
		s.cfg.Log.Info("killed session %s", id)
	}
}

func (s *SessionStore) index() []byte {
	var sb strings.Builder
	s.mu.RLock()
	ids := make([]string, 0, len(s.sessions))
	for id := range s.sessions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		sess := s.sessions[id]
		sess.mu.RLock()
		state := sess.Core.State()
		cwd := sess.Core.CWD()
		be := sess.Core.BackendName()
		model := sess.Core.ModelName()
		sess.mu.RUnlock()
		fmt.Fprintf(&sb, "%s\t%s\t%s\t%s\t%s\n", id, state, cwd, be, model)
	}
	s.mu.RUnlock()
	return []byte(sb.String())
}

func (s *SessionStore) createSession(args []string) error {
	name := ""
	backendOverride := ""
	modelOverride := ""
	agentName := "default"
	cwd := ""
	for _, arg := range args {
		k, v, ok := strings.Cut(arg, "=")
		if !ok {
			return fmt.Errorf("invalid option %q (expected key=value)", arg)
		}
		if v == "" {
			continue
		}
		switch k {
		case "name":
			name = v
		case "backend":
			backendOverride = v
		case "model":
			modelOverride = v
		case "agent":
			agentName = v
		case "cwd":
			cwd = v
		default:
			return fmt.Errorf("unknown option %q (valid: name, backend, model, agent, cwd)", k)
		}
	}

	if cwd == "" {
		return fmt.Errorf("cwd is required (e.g. new cwd=/path/to/project)")
	}

	sessID := name
	if sessID == "" {
		sessID = agent.NewSessionID()
	}

	s.mu.RLock()
	_, exists := s.sessions[sessID]
	s.mu.RUnlock()
	if exists {
		return fmt.Errorf("session already exists: %s", sessID)
	}

	var core agent.Core
	if s.cfg.NewCore != nil {
		var err error
		core, err = s.cfg.NewCore(sessID, agentName, cwd)
		if err != nil {
			return err
		}
	} else {
		be, err := backend.NewWithName(backendOverride)
		if err != nil {
			return fmt.Errorf("backend: %w", err)
		}

		if modelOverride == "" {
			modelOverride = os.Getenv("OLLIE_MODEL")
		}
		if modelOverride != "" {
			be.SetModel(modelOverride)
		}

		if err := s.cfg.MkdirAll(s.cfg.SessionsDir, 0700); err != nil {
			return fmt.Errorf("sessions dir: %w", err)
		}

		cfg := LoadAgentConfig(s.cfg.AgentsDir, agentName, nil)

		newDisp := tools.NewDispatcherFunc(map[string]func() tools.Server{
			"execute": execute.Decl(cwd),
		})

		env := agent.BuildAgentEnv(cfg, newDisp(), cwd)

		core = agent.NewAgentCore(agent.AgentCoreConfig{
			Backend:       be,
			AgentName:     agentName,
			AgentsDir:     s.cfg.AgentsDir,
			SessionsDir:   s.cfg.SessionsDir,
			SessionID:     sessID,
			CWD:           cwd,
			Env:           env,
			NewDispatcher: newDisp,
			Log:           s.cfg.Sink.NewLogger("core"),
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	sess := NewSession(sessID, core, ctx, cancel)

	s.mu.Lock()
	s.sessions[sessID] = sess
	s.mu.Unlock()

	s.cfg.Log.Info("new session %s (backend=%s model=%s agent=%s)",
		sessID, core.BackendName(), core.ModelName(), core.AgentName())
	return nil
}

func (s *SessionStore) renameSession(oldID, newID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	sess, ok := s.sessions[oldID]
	if !ok {
		return fmt.Errorf("session not found: %s", oldID)
	}
	if _, exists := s.sessions[newID]; exists {
		return fmt.Errorf("session already exists: %s", newID)
	}
	if sess.Core.IsRunning() {
		return fmt.Errorf("cannot rename while agent is running")
	}

	if err := sess.Core.SetSessionID(newID); err != nil {
		return err
	}

	sess.id = newID
	s.sessions[newID] = sess
	delete(s.sessions, oldID)

	if s.cfg.OnRename != nil {
		s.cfg.OnRename(oldID, newID)
	}

	sess.AppendLog([]byte(fmt.Sprintf(":: session renamed: %s -> %s\n", oldID, newID)))
	s.cfg.Log.Info("renamed session %s -> %s", oldID, newID)
	return nil
}

// LoadAgentConfig resolves and loads the config for a named agent.
// Returns nil if the config file does not exist; BuildAgentEnv handles nil configs.
func LoadAgentConfig(agentsDir, name string, open func(string) (*os.File, error)) *config.Config {
	if open == nil {
		open = os.Open
	}
	f, err := open(agentsDir + "/" + name + ".json")
	if err != nil {
		return nil
	}
	defer f.Close()
	cfg, _ := config.Load(f)
	return cfg
}

// FormatEvent converts an agent Event to bytes for appending to a chat log.
func FormatEvent(ev agent.Event) []byte {
	switch ev.Role {
	case "user":
		return []byte("user: " + ev.Content + "\n")
	case "assistant":
		return []byte(ev.Content)
	case "call":
		args := squashWhitespace(ev.Content)
		return []byte("-> " + ev.Name + "(" + args + ")\n")
	case "tool":
		return []byte(strings.TrimRight(ev.Content, "\n") + "\n")
	case "retry":
		return []byte("retrying in " + ev.Content + "s...\n")
	case "error":
		return []byte("error: " + ev.Content + "\n")
	case "stalled":
		return []byte("agent stalled\n")
	case "reasoning":
		return []byte(ev.Content)
	case "info":
		return []byte(":: " + ev.Content)
	default:
		return nil
	}
}

func squashWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// Package p9 implements the 9P filesystem for ollie sessions.
//
// Filesystem layout:
//
//	ollie/
//	    a/                  (dir)   agent configs (r/w, backed by ~/.config/ollie/agents/)
//	    backends            (read)  list of ollie-provided backends
//	    help                (read)  help file (backed by ~/.config/ollie/help.md)
//	    m/                  (dir)   memories (r/w, backed by OLLIE_MEMORY_PATH)
//	    p/                  (dir)   prompt templates
//	    pl/                 (dir)   plans
//	    s/                  (dir)   session directory
//	        new             (r/w)   read: KV template; write: create session
//	        idx             (read)  live index: name state cwd backend model (tab-separated)
//	        {session-id}/           rm -r to kill session; mv to rename
//	            ctl         (write) session control: stop, kill, rn, compact, clear
//	            prompt      (write) submit a prompt to the agent
//	            enqueue     (write) queue a prompt for later execution
//	            dequeue     (read)  pop the next queued prompt
//	            chat        (read)  cumulative chat history
//	            reply       (read)  assistant text from the most recent turn
//	            state       (read)  current agent state
//	            backend     (r/w)   active backend name
//	            agent       (r/w)   active agent name
//	            model       (r/w)   active model name
//	            models      (read)  available models from the backend
//	            mcp         (read)  MCP server list
//	            cwd         (r/w)   working directory
//	            usage       (read)  token counts
//	            ctxsz       (read)  context size vs window
//	    sk/                 (dir)   skills
//	    t/                  (dir)   tool scripts
package p9

import (
	"context"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"9fans.net/go/plan9"
	"ollie/pkg/agent"
	"ollie/pkg/backend"
	"ollie/pkg/config"
	"ollie/pkg/tools"
	"ollie/pkg/tools/execute"
)

const (
	QTDir  = plan9.QTDIR
	QTFile = plan9.QTFILE
)

// mutableFile tracks version and last-known value for a tailable session file.
// Qid.Vers is bumped on change so tail -f detects rewrites via stat polling.
// On change, truncated is set to 1 so the next stat reports Length 0,
// causing tail to detect truncation and re-read from offset 0.
type mutableFile struct {
	vers      uint32
	last      string
	truncated int32 // atomic: 1 after change, cleared by stat
}

func (m *mutableFile) update(val string) {
	if val != m.last {
		m.last = val
		m.vers++
		atomic.StoreInt32(&m.truncated, 1)
	}
}

// session holds all state for one ollie agent session.
type session struct {
	mu     sync.RWMutex
	id     string
	core   agent.Core
	ctx    context.Context
	cancel context.CancelFunc
	// chatLog is an append-only record of the conversation. Reads are served
	// directly from this buffer by offset, so tail -f works via polling.
	chatLog  []byte
	chatVers uint32 // incremented on each append; used as Qid.Vers
	// replyVers is incremented on each turn to drive Qid.Vers for polling.
	replyVers uint32
	// mutableVers tracks changes to tailable mutable-state files
	// (state, backend, agent, model, usage, cwd). Bumped on change;
	// reported via Qid.Vers in stat so tail -f detects truncation/rewrite.
	mutableVers map[string]*mutableFile
}


// appendChat appends data to the session's chat log and bumps the Qid version.
func (sess *session) appendChat(data []byte) {
	if len(data) == 0 {
		return
	}
	sess.mu.Lock()
	sess.chatLog = append(sess.chatLog, data...)
	sess.chatVers++
	sess.mu.Unlock()
}

// trackMutable snapshots state and usage into their version trackers.
// Called from the publish callback after each event.
func (sess *session) trackMutable() {
	state := sess.core.State()
	usage := sess.core.Usage()
	sess.mu.Lock()
	sess.mutableVers["state"].update(state)
	sess.mutableVers["usage"].update(usage)
	sess.mu.Unlock()
}

// fid tracks per-descriptor state for a single 9P connection.
type fid struct {
	path     string
	qid      plan9.Qid
	mode     uint8
	writeBuf []byte
}

// connState tracks all open fids for a single 9P connection.
type connState struct {
	mu   sync.RWMutex
	fids map[uint32]*fid
}

// Server is the 9P server for ollie sessions.
type Server struct {
	mu          sync.RWMutex
	sessions    map[string]*session
	conns       []*connState
	agentsDir   string // kept for session creation wiring
	sessionsDir string
	agentStore  Store
	promptStore ReadableStore
	planStore   Store
	memStore    Store
	toolStore    Store
	skillStore   Store
	sessionStore Store
}

// New creates a new Server.
func New() *Server {
	memDir := defaultMemDir()
	os.MkdirAll(memDir, 0755) //nolint:errcheck
	agentsDir := agent.DefaultAgentsDir()
	s := &Server{
		sessions:    make(map[string]*session),
		agentsDir:   agentsDir,
		sessionsDir: agent.DefaultSessionsDir(),
		agentStore:  NewFlatDirStore(agentsDir, 0644),
		promptStore: NewFlatDirStore(agent.DefaultPromptsDir(), 0444),
		planStore:   NewFlatDirStore(defaultPlanDir(), 0644),
		memStore:    NewFlatDirStore(memDir, 0644),
		toolStore:   NewToolStore(),
		skillStore:  NewSkillStore(),
	}
	s.sessionStore = &SessionStore{srv: s}
	return s
}

// defaultPlanDir returns the planning directory from OLLIE_PLAN_PATH or the default.
func defaultPlanDir() string {
	if p := os.Getenv("OLLIE_PLAN_PATH"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return home + "/.config/ollie/planning"
}

// defaultMemDir returns the memory directory from OLLIE_MEMORY_PATH or the default.
func defaultMemDir() string {
	if p := os.Getenv("OLLIE_MEMORY_PATH"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return home + "/.config/ollie/memory"
}

// sessionFileStore returns a SessionFileStore for the given session ID.
func (s *Server) sessionFileStore(sessID string) (*SessionFileStore, bool) {
	s.mu.RLock()
	sess, ok := s.sessions[sessID]
	s.mu.RUnlock()
	if !ok {
		return nil, false
	}
	return NewSessionFileStore(
		sess,
		func() { s.killSession(sessID) },
		func(newID string) error { return s.renameSession(sessID, newID) },
	), true
}

// Serve handles a single 9P connection.
func (s *Server) Serve(conn net.Conn) {
	defer conn.Close()
	cs := &connState{fids: make(map[uint32]*fid)}
	s.mu.Lock()
	s.conns = append(s.conns, cs)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		for i, c := range s.conns {
			if c == cs {
				s.conns = append(s.conns[:i], s.conns[i+1:]...)
				break
			}
		}
		s.mu.Unlock()
	}()
	for {
		fc, err := plan9.ReadFcall(conn)
		if err != nil {
			if err != io.EOF {
				fmt.Fprintf(os.Stderr, "olliesrv: read: %v\n", err)
			}
			return
		}
		plan9.WriteFcall(conn, s.handle(cs, fc)) //nolint:errcheck
	}
}

func (s *Server) handle(cs *connState, fc *plan9.Fcall) *plan9.Fcall {
	switch fc.Type {
	case plan9.Tversion:
		msize := fc.Msize
		if msize > 65536 {
			msize = 65536
		}
		return &plan9.Fcall{Type: plan9.Rversion, Tag: fc.Tag, Msize: msize, Version: "9P2000"}
	case plan9.Tauth:
		return errFcall(fc, "no auth required")
	case plan9.Tattach:
		return s.attach(cs, fc)
	case plan9.Twalk:
		return s.walk(cs, fc)
	case plan9.Topen:
		return s.open(cs, fc)
	case plan9.Tcreate:
		return s.create(cs, fc)
	case plan9.Tread:
		return s.read(cs, fc)
	case plan9.Twrite:
		return s.write(cs, fc)
	case plan9.Tstat:
		return s.stat(cs, fc)
	case plan9.Twstat:
		return s.wstat(cs, fc)
	case plan9.Tflush:
		// No blocking reads to cancel; always succeed.
		return &plan9.Fcall{Type: plan9.Rflush, Tag: fc.Tag}
	case plan9.Tclunk:
		return s.clunk(cs, fc)
	case plan9.Tremove:
		return s.remove(cs, fc)
	default:
		return errFcall(fc, "unsupported operation")
	}
}

func errFcall(fc *plan9.Fcall, msg string) *plan9.Fcall {
	return &plan9.Fcall{Type: plan9.Rerror, Tag: fc.Tag, Ename: msg}
}

// qidPath returns a stable numeric path for use in Qid structs.
func qidPath(path string) uint64 {
	if path == "/" {
		return 0
	}
	h := fnv.New64a()
	h.Write([]byte(path))
	return h.Sum64()
}

// pathType returns "dir", "file", or "" (not found) for a logical path.
func (s *Server) pathType(path string) string {
	if path == "/" {
		return "dir"
	}
	trimmed := strings.TrimPrefix(path, "/")
	parts := strings.SplitN(trimmed, "/", 3)
	switch {
	case len(parts) == 1 && parts[0] == "s":
		return "dir"
	case len(parts) == 2 && parts[0] == "s":
		if info, err := s.sessionStore.Stat(parts[1]); err == nil {
			if info.IsDir() {
				return "dir"
			}
			return "file"
		}
	case len(parts) == 1 && parts[0] == "a":
		return "dir"
	case len(parts) == 1 && parts[0] == "p":
		return "dir"
	case len(parts) == 1 && parts[0] == "m":
		return "dir"
	case len(parts) == 1 && parts[0] == "pl":
		return "dir"
	case len(parts) == 1 && parts[0] == "sk":
		return "dir"
	case len(parts) == 1 && parts[0] == "t":
		return "dir"
	case len(parts) == 1 && parts[0] == "backends":
		return "file"
	case len(parts) == 1 && parts[0] == "help":
		return "file"
	case len(parts) == 2 && parts[0] == "a":
		if _, err := s.agentStore.Stat(parts[1]); err == nil {
			return "file"
		}
	case len(parts) == 2 && parts[0] == "p":
		if _, err := s.promptStore.Stat(parts[1]); err == nil {
			return "file"
		}
	case len(parts) == 2 && parts[0] == "m":
		if _, err := s.memStore.Stat(parts[1]); err == nil {
			return "file"
		}
	case len(parts) == 2 && parts[0] == "pl":
		if _, err := s.planStore.Stat(parts[1]); err == nil {
			return "file"
		}
	case len(parts) == 2 && parts[0] == "sk":
		if _, err := s.skillStore.Stat(parts[1]); err == nil {
			return "file"
		}
	case len(parts) == 2 && parts[0] == "t":
		if _, err := s.toolStore.Stat(parts[1]); err == nil {
			return "file"
		}
	case len(parts) == 3 && parts[0] == "s":
		if store, ok := s.sessionFileStore(parts[1]); ok {
			if _, err := store.Stat(parts[2]); err == nil {
				return "file"
			}
		}
	}
	return ""
}

func pathParent(path string) string {
	if path == "/" {
		return "/"
	}
	i := strings.LastIndex(path, "/")
	if i == 0 {
		return "/"
	}
	return path[:i]
}

func pathJoin(parent, name string) string {
	if parent == "/" {
		return "/" + name
	}
	return parent + "/" + name
}

func pathBase(path string) string {
	i := strings.LastIndex(path, "/")
	if i < 0 {
		return path
	}
	return path[i+1:]
}

func (s *Server) attach(cs *connState, fc *plan9.Fcall) *plan9.Fcall {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	qid := plan9.Qid{Type: QTDir, Path: 0}
	cs.fids[fc.Fid] = &fid{path: "/", qid: qid}
	return &plan9.Fcall{Type: plan9.Rattach, Tag: fc.Tag, Qid: qid}
}

func (s *Server) walk(cs *connState, fc *plan9.Fcall) *plan9.Fcall {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	f, ok := cs.fids[fc.Fid]
	if !ok {
		return errFcall(fc, "bad fid")
	}

	// Clone: copy the fid to newfid (even when wnames is empty).
	newf := &fid{path: f.path, qid: f.qid}

	if len(fc.Wname) == 0 {
		cs.fids[fc.Newfid] = newf
		return &plan9.Fcall{Type: plan9.Rwalk, Tag: fc.Tag, Wqid: []plan9.Qid{}}
	}

	wqids := make([]plan9.Qid, 0, len(fc.Wname))
	cur := f.path

	for _, name := range fc.Wname {
		var next string
		if name == ".." {
			next = pathParent(cur)
		} else {
			next = pathJoin(cur, name)
		}

		t := s.pathType(next)
		if t == "" {
			if len(wqids) == 0 {
				return errFcall(fc, name+": file not found")
			}
			break
		}

		q := plan9.Qid{Path: qidPath(next)}
		if t == "dir" {
			q.Type = QTDir
		}
		wqids = append(wqids, q)
		cur = next
	}

	if len(wqids) == len(fc.Wname) {
		newf.path = cur
		newf.qid = wqids[len(wqids)-1]
		cs.fids[fc.Newfid] = newf
	}

	return &plan9.Fcall{Type: plan9.Rwalk, Tag: fc.Tag, Wqid: wqids}
}

func (s *Server) open(cs *connState, fc *plan9.Fcall) *plan9.Fcall {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	f, ok := cs.fids[fc.Fid]
	if !ok {
		return errFcall(fc, "bad fid")
	}

	f.mode = fc.Mode
	return &plan9.Fcall{Type: plan9.Ropen, Tag: fc.Tag, Qid: f.qid}
}

func (s *Server) create(cs *connState, fc *plan9.Fcall) *plan9.Fcall {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	f, ok := cs.fids[fc.Fid]
	if !ok {
		return errFcall(fc, "bad fid")
	}
	if fc.Perm&plan9.DMDIR != 0 {
		return errFcall(fc, "mkdir not supported")
	}

	newPath := pathJoin(f.path, fc.Name)

	// Actually create the file on disk so that even a no-write create
	// (e.g. touch) produces a real file.
	switch f.path {
	case "/a":
		if err := s.agentStore.Create(fc.Name); err != nil {
			return errFcall(fc, err.Error())
		}
	case "/m":
		if err := s.memStore.Create(fc.Name); err != nil {
			return errFcall(fc, err.Error())
		}
	case "/pl":
		if err := s.planStore.Create(fc.Name); err != nil {
			return errFcall(fc, err.Error())
		}
	case "/t":
		if err := s.toolStore.Create(fc.Name); err != nil {
			return errFcall(fc, err.Error())
		}
	case "/sk":
		if err := s.skillStore.Create(fc.Name); err != nil {
			return errFcall(fc, err.Error())
		}
	}

	qid := plan9.Qid{Path: qidPath(newPath)}
	f.path = newPath
	f.qid = qid
	f.mode = fc.Mode
	return &plan9.Fcall{Type: plan9.Rcreate, Tag: fc.Tag, Qid: qid}
}

func (s *Server) read(cs *connState, fc *plan9.Fcall) *plan9.Fcall {
	cs.mu.RLock()
	f, ok := cs.fids[fc.Fid]
	if !ok {
		cs.mu.RUnlock()
		return errFcall(fc, "bad fid")
	}
	path := f.path
	isDir := f.qid.Type&QTDir != 0
	cs.mu.RUnlock()

	if isDir {
		data := s.readDir(path, fc.Offset, fc.Count)
		return &plan9.Fcall{Type: plan9.Rread, Tag: fc.Tag, Count: uint32(len(data)), Data: data}
	}

	// s/new and s/idx are served from the session store.
	if path == "/s/new" || path == "/s/idx" {
		content, err := s.sessionStore.Get(pathBase(path))
		if err != nil {
			return errFcall(fc, err.Error())
		}
		return s.readSlice(fc, content)
	}

	// backends is a static list of ollie-provided backends.
	if path == "/backends" {
		content := []byte(strings.Join(backend.Backends(), "\n") + "\n")
		return s.readSlice(fc, content)
	}

	// help is served from ~/.config/ollie/help.md.
	if path == "/help" {
		content, err := os.ReadFile(s.helpPath())
		if err != nil {
			return errFcall(fc, err.Error())
		}
		return s.readSlice(fc, content)
	}

	// Agent config files are served from the agent store.
	if strings.HasPrefix(path, "/a/") {
		content, err := s.agentStore.Get(pathBase(path))
		if err != nil {
			return errFcall(fc, err.Error())
		}
		return s.readSlice(fc, content)
	}

	// Prompt files are served from the prompt store.
	if strings.HasPrefix(path, "/p/") {
		content, err := s.promptStore.Get(pathBase(path))
		if err != nil {
			return errFcall(fc, err.Error())
		}
		return s.readSlice(fc, content)
	}

	// Memory files are served from the memory store.
	if strings.HasPrefix(path, "/m/") {
		content, err := s.memStore.Get(pathBase(path))
		if err != nil {
			return errFcall(fc, err.Error())
		}
		return s.readSlice(fc, content)
	}

	// Plan files are served from the plan store.
	if strings.HasPrefix(path, "/pl/") {
		content, err := s.planStore.Get(pathBase(path))
		if err != nil {
			return errFcall(fc, err.Error())
		}
		return s.readSlice(fc, content)
	}

	// Skill files are served from the skill store.
	if strings.HasPrefix(path, "/sk/") {
		content, err := s.skillStore.Get(pathBase(path))
		if err != nil {
			return errFcall(fc, err.Error())
		}
		return s.readSlice(fc, content)
	}

	// Tool files are served from the tool store.
	if strings.HasPrefix(path, "/t/") {
		content, err := s.toolStore.Get(pathBase(path))
		if err != nil {
			return errFcall(fc, err.Error())
		}
		return s.readSlice(fc, content)
	}

	// Session files: /s/{id}/{file}
	if strings.HasPrefix(path, "/s/") {
		parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 3)
		if len(parts) == 3 {
			store, ok := s.sessionFileStore(parts[1])
			if !ok {
				return &plan9.Fcall{Type: plan9.Rread, Tag: fc.Tag, Count: 0}
			}
			// dequeue: non-zero offset is the trailing EOF read after a successful pop.
			if parts[2] == "dequeue" && fc.Offset > 0 {
				return &plan9.Fcall{Type: plan9.Rread, Tag: fc.Tag, Count: 0}
			}
			content, err := store.Get(parts[2])
			if err != nil {
				return errFcall(fc, err.Error())
			}
			return s.readSlice(fc, content)
		}
	}
	return &plan9.Fcall{Type: plan9.Rread, Tag: fc.Tag, Count: 0}
}


// readSlice serves a byte slice at the requested offset/count.
func (s *Server) readSlice(fc *plan9.Fcall, content []byte) *plan9.Fcall {
	var data []byte
	off := int(fc.Offset)
	if off < len(content) {
		end := off + int(fc.Count)
		if end > len(content) {
			end = len(content)
		}
		data = content[off:end]
	}
	return &plan9.Fcall{Type: plan9.Rread, Tag: fc.Tag, Count: uint32(len(data)), Data: data}
}

func (s *Server) helpPath() string {
	home, _ := os.UserHomeDir()
	return home + "/.config/ollie/help.md"
}


func (s *Server) write(cs *connState, fc *plan9.Fcall) *plan9.Fcall {
	cs.mu.Lock()
	f, ok := cs.fids[fc.Fid]
	if !ok {
		cs.mu.Unlock()
		return errFcall(fc, "bad fid")
	}
	// Accumulate; the 9P client may split large writes across multiple Twrite messages.
	end := int(fc.Offset) + len(fc.Data)
	if end > len(f.writeBuf) {
		grown := make([]byte, end)
		copy(grown, f.writeBuf)
		f.writeBuf = grown
	}
	copy(f.writeBuf[fc.Offset:], fc.Data)
	cs.mu.Unlock()
	return &plan9.Fcall{Type: plan9.Rwrite, Tag: fc.Tag, Count: uint32(len(fc.Data))}
}

func (s *Server) stat(cs *connState, fc *plan9.Fcall) *plan9.Fcall {
	cs.mu.RLock()
	f, ok := cs.fids[fc.Fid]
	cs.mu.RUnlock()
	if !ok {
		return errFcall(fc, "bad fid")
	}
	dir := s.makeStat(f.path)
	stat, err := dir.Bytes()
	if err != nil {
		return errFcall(fc, err.Error())
	}
	return &plan9.Fcall{Type: plan9.Rstat, Tag: fc.Tag, Stat: stat}
}

func (s *Server) wstat(cs *connState, fc *plan9.Fcall) *plan9.Fcall {
	cs.mu.Lock()
	f, ok := cs.fids[fc.Fid]
	cs.mu.Unlock()
	if !ok {
		return errFcall(fc, "bad fid")
	}

	// Parse the new Dir from the stat bytes.
	newDir, err := plan9.UnmarshalDir(fc.Stat)
	if err != nil {
		// Some clients send minimal wstat (e.g. truncate); accept silently.
		return &plan9.Fcall{Type: plan9.Rwstat, Tag: fc.Tag}
	}

	oldName := pathBase(f.path)
	if newDir.Name == "" || newDir.Name == oldName {
		return &plan9.Fcall{Type: plan9.Rwstat, Tag: fc.Tag}
	}

	switch {
	case strings.HasPrefix(f.path, "/a/"):
		if err := s.agentStore.Rename(oldName, newDir.Name); err != nil {
			return errFcall(fc, err.Error())
		}
		cs.mu.Lock()
		f.path = "/a/" + newDir.Name
		f.qid.Path = qidPath(f.path)
		cs.mu.Unlock()

	case strings.HasPrefix(f.path, "/m/"):
		if err := s.memStore.Rename(oldName, newDir.Name); err != nil {
			return errFcall(fc, err.Error())
		}
		cs.mu.Lock()
		f.path = "/m/" + newDir.Name
		f.qid.Path = qidPath(f.path)
		cs.mu.Unlock()

	case strings.HasPrefix(f.path, "/pl/"):
		if err := s.planStore.Rename(oldName, newDir.Name); err != nil {
			return errFcall(fc, err.Error())
		}
		cs.mu.Lock()
		f.path = "/pl/" + newDir.Name
		f.qid.Path = qidPath(f.path)
		cs.mu.Unlock()

	case strings.HasPrefix(f.path, "/sk/"):
		if err := s.skillStore.Rename(oldName, newDir.Name); err != nil {
			return errFcall(fc, err.Error())
		}
		cs.mu.Lock()
		f.path = "/sk/" + newDir.Name
		f.qid.Path = qidPath(f.path)
		cs.mu.Unlock()

	case strings.HasPrefix(f.path, "/t/"):
		if err := s.toolStore.Rename(oldName, newDir.Name); err != nil {
			return errFcall(fc, err.Error())
		}
		cs.mu.Lock()
		f.path = "/t/" + newDir.Name
		f.qid.Path = qidPath(f.path)
		cs.mu.Unlock()

	case strings.HasPrefix(f.path, "/s/") && f.path != "/s/new":
		// Session directory rename: /s/{old} -> /s/{new}
		parts := strings.SplitN(strings.TrimPrefix(f.path, "/"), "/", 3)
		if len(parts) != 2 || parts[0] != "s" {
			return errFcall(fc, "rename not supported")
		}
		if err := s.sessionStore.Rename(parts[1], newDir.Name); err != nil {
			return errFcall(fc, err.Error())
		}

	default:
		return &plan9.Fcall{Type: plan9.Rwstat, Tag: fc.Tag}
	}

	return &plan9.Fcall{Type: plan9.Rwstat, Tag: fc.Tag}
}

// renameSession re-keys a session in the sessions map, updates the core's
// session ID, and rewrites fid paths across all connections.
// Caller must hold NO locks on s.mu (this method acquires it).
func (s *Server) renameSession(oldID, newID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	sess, ok := s.sessions[oldID]
	if !ok {
		return fmt.Errorf("session not found: %s", oldID)
	}
	if _, exists := s.sessions[newID]; exists {
		return fmt.Errorf("session already exists: %s", newID)
	}
	if sess.core.IsRunning() {
		return fmt.Errorf("cannot rename while agent is running")
	}

	if err := sess.core.SetSessionID(newID); err != nil {
		return err
	}

	sess.id = newID
	s.sessions[newID] = sess
	delete(s.sessions, oldID)

	// Rewrite fid paths across all connections.
	oldPrefix := "/s/" + oldID
	newPrefix := "/s/" + newID
	for _, c := range s.conns {
		c.mu.Lock()
		for _, f := range c.fids {
			if f.path == oldPrefix || strings.HasPrefix(f.path, oldPrefix+"/") {
				f.path = newPrefix + f.path[len(oldPrefix):]
				f.qid.Path = qidPath(f.path)
			}
		}
		c.mu.Unlock()
	}

	// Rename the session tmpdir so isread markers remain valid.
	oldTemp := "/tmp/ollie/" + oldID
	newTemp := "/tmp/ollie/" + newID
	if _, err := os.Stat(oldTemp); err == nil {
		os.Rename(oldTemp, newTemp) //nolint:errcheck
	}

	sess.appendChat([]byte(fmt.Sprintf("(session renamed: %s -> %s)\n", oldID, newID)))
	fmt.Fprintf(os.Stderr, "olliesrv: renamed session %s -> %s\n", oldID, newID)
	return nil
}

func (s *Server) clunk(cs *connState, fc *plan9.Fcall) *plan9.Fcall {
	cs.mu.Lock()
	f, ok := cs.fids[fc.Fid]
	var path string
	var data []byte
	if ok {
		if len(f.writeBuf) > 0 {
			path = f.path
			data = make([]byte, len(f.writeBuf))
			copy(data, f.writeBuf)
		}
		delete(cs.fids, fc.Fid)
	}
	cs.mu.Unlock()
	if len(data) > 0 {
		input := strings.TrimSpace(string(data))
		if s.isAsyncWrite(path) {
			go s.handleWrite(path, input) //nolint:errcheck
		} else if err := s.handleWrite(path, input); err != nil {
			return errFcall(fc, err.Error())
		}
	}
	return &plan9.Fcall{Type: plan9.Rclunk, Tag: fc.Tag}
}

// isAsyncWrite returns true for paths where writes may block (agent turns)
// and Rerror is not useful.
func (s *Server) isAsyncWrite(path string) bool {
	if !strings.HasPrefix(path, "/s/") {
		return false
	}
	switch pathBase(path) {
	case "prompt", "enqueue", "ctl":
		return true
	}
	return false
}

func (s *Server) remove(cs *connState, fc *plan9.Fcall) *plan9.Fcall {
	cs.mu.Lock()
	f, ok := cs.fids[fc.Fid]
	if ok {
		delete(cs.fids, fc.Fid)
	}
	cs.mu.Unlock()
	if !ok {
		return errFcall(fc, "bad fid")
	}

	path := f.path
	var err error
	switch {
	case strings.HasPrefix(path, "/a/"):
		err = s.agentStore.Delete(pathBase(path))
	case strings.HasPrefix(path, "/m/"):
		err = s.memStore.Delete(pathBase(path))
	case strings.HasPrefix(path, "/pl/"):
		err = s.planStore.Delete(pathBase(path))
	case strings.HasPrefix(path, "/sk/"):
		err = s.skillStore.Delete(pathBase(path))
	case strings.HasPrefix(path, "/t/"):
		err = s.toolStore.Delete(pathBase(path))
	case strings.HasPrefix(path, "/s/") && path != "/s/new":
		// If path is /s/{id}/{file}, it's a synthetic session file — no-op so
		// that "rm -r s/{id}" can proceed to remove the directory itself, which
		// triggers the actual session kill via Delete({id}).
		parts := strings.SplitN(strings.TrimPrefix(path, "/s/"), "/", 2)
		if len(parts) == 2 {
			err = nil // synthetic file; let rm -r continue
		} else {
			err = s.sessionStore.Delete(parts[0])
		}
	default:
		return errFcall(fc, "remove not supported")
	}
	if err != nil {
		return errFcall(fc, err.Error())
	}
	return &plan9.Fcall{Type: plan9.Rremove, Tag: fc.Tag}
}

// handleWrite processes a fully-assembled write payload for the given path.
// Called synchronously from clunk; prompt writes are the exception (spawned
// as a goroutine because they block for the entire agent turn).
func (s *Server) handleWrite(path, input string) error {
	if input == "" {
		return nil
	}

	if path == "/s/new" {
		return s.sessionStore.Put("new", []byte(input))
	}

	// Agent config writes go to the agent store.
	if strings.HasPrefix(path, "/a/") {
		return s.agentStore.Put(pathBase(path), []byte(input))
	}

	// Memory file writes go to the memory store.
	if strings.HasPrefix(path, "/m/") {
		return s.memStore.Put(pathBase(path), []byte(input))
	}

	// Plan file writes go to the plan store.
	if strings.HasPrefix(path, "/pl/") {
		return s.planStore.Put(pathBase(path), []byte(input))
	}

	// Skill file writes go to the skill store.
	if strings.HasPrefix(path, "/sk/") {
		return s.skillStore.Put(pathBase(path), []byte(input))
	}

	// Tool file writes go to the tool store.
	if strings.HasPrefix(path, "/t/") {
		return s.toolStore.Put(pathBase(path), []byte(input))
	}

	// Session file writes: /s/{sessid}/{file}
	if strings.HasPrefix(path, "/s/") {
		parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 3)
		if len(parts) == 3 {
			store, ok := s.sessionFileStore(parts[1])
			if !ok {
				return fmt.Errorf("session not found: %s", parts[1])
			}
			return store.Put(parts[2], []byte(input))
		}
	}

	return nil
}

// handleNewSession parses KV pairs (one per line or space-separated) and creates a session.
func (s *Server) handleNewSession(input string) error {
	// Accept both newline-separated and space-separated KV pairs.
	args := strings.Fields(input)
	return s.createSession(args)
}

// createSession parses key=value options and starts a new agent session.
// Recognised keys: name, backend, model, agent, cwd. cwd is required.
// Example: new cwd=/home/lkn/src/myproject backend=ollama model=qwen3:8b agent=myagent
func (s *Server) createSession(args []string) error {
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

	var (
		be  backend.Backend
		err error
	)
	if backendOverride != "" {
		old := os.Getenv("OLLIE_BACKEND")
		os.Setenv("OLLIE_BACKEND", backendOverride) //nolint:errcheck
		be, err = backend.New()
		os.Setenv("OLLIE_BACKEND", old) //nolint:errcheck
	} else {
		be, err = backend.New()
	}
	if err != nil {
		return fmt.Errorf("backend: %w", err)
	}

	if modelOverride != "" {
		be.SetModel(modelOverride)
	}

	if err := os.MkdirAll(s.sessionsDir, 0700); err != nil {
		return fmt.Errorf("sessions dir: %w", err)
	}

	cfgPath := agent.AgentConfigPath(s.agentsDir, agentName)
	cfg, _ := config.Load(cfgPath) // nil cfg is handled by BuildAgentEnv

	newDisp := tools.NewDispatcherFunc(map[string]func() tools.Server{
		"execute":   execute.Decl(cwd),
	})

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

	env := agent.BuildAgentEnv(cfg, newDisp(), cwd)

	// Inject per-session env vars into the execute server.
	if srv, ok := env.Dispatcher().GetServer("execute"); ok {
		if es, ok := srv.(tools.EnvSetter); ok {
			es.SetEnv("OLLIE_SESSION_ID", sessID)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	core := agent.NewAgentCore(agent.AgentCoreConfig{
		Backend:       be,
		AgentName:     agentName,
		AgentsDir:     s.agentsDir,
		SessionsDir:   s.sessionsDir,
		SessionID:     sessID,
		CWD:           cwd,
		Env:           env,
		NewDispatcher: newDisp,
	})

	sess := &session{
		id:     sessID,
		core:   core,
		ctx:    ctx,
		cancel: cancel,
		mutableVers: map[string]*mutableFile{
			"state":   {last: core.State()},
			"backend": {last: core.BackendName()},
			"agent":   {last: core.AgentName()},
			"model":   {last: core.ModelName()},
			"usage":   {last: core.Usage()},
			"cwd": {last: core.CWD()},
		},
	}

	s.mu.Lock()
	s.sessions[sessID] = sess
	s.mu.Unlock()

	fmt.Fprintf(os.Stderr, "olliesrv: new session %s (backend=%s model=%s agent=%s)\n",
		sessID, core.BackendName(), core.ModelName(), core.AgentName())
	return nil
}

// Shutdown kills all active sessions, triggering Close() on each core.
func (s *Server) Shutdown() {
	s.mu.Lock()
	ids := make([]string, 0, len(s.sessions))
	for id := range s.sessions {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.killSession(id)
	}
}

func (s *Server) killSession(id string) {
	s.mu.Lock()
	sess := s.sessions[id]
	delete(s.sessions, id)
	s.mu.Unlock()
	if sess != nil {
		sess.cancel()
		sess.core.Close()
		fmt.Fprintf(os.Stderr, "olliesrv: killed session %s\n", id)
	}
}



// formatEvent converts an agent Event to bytes for appending to the chat log.
// Output matches ollie-tui's MakeOutputFn so the chat file looks identical to
// what appears in the TUI chat pane.
func formatEvent(ev agent.Event) []byte {
	switch ev.Role {
	case "user":
		return []byte("user: " + ev.Content + "\n")
	case "assistant":
		return []byte(ev.Content)
	case "call":
		args := squashWhitespace(ev.Content)
		if len(args) > 500 {
			args = args[:500] + "..."
		}
		return []byte("-> " + ev.Name + "(" + args + ")\n")
	case "tool":
		return []byte(strings.TrimRight(ev.Content, "\n") + "\n")
	case "retry":
		return []byte("retrying in " + ev.Content + "s...\n")
	case "error":
		return []byte("error: " + ev.Content + "\n")
	case "stalled":
		return []byte("agent stalled\n")
	case "info":
		return []byte(ev.Content)
	case "newline":
		return []byte("\n")
	default:
		return nil
	}
}

func squashWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// readDir serializes directory entries for the given path, respecting offset and count.
func (s *Server) readDir(path string, offset uint64, count uint32) []byte {
	var dirs []plan9.Dir

	makeDir := func(name, fpath string, isDir bool, mode plan9.Perm) plan9.Dir {
		q := plan9.Qid{Path: qidPath(fpath)}
		if isDir {
			q.Type = QTDir
		}
		return plan9.Dir{Qid: q, Mode: mode, Name: name, Uid: "ollie", Gid: "ollie", Muid: "ollie"}
	}

	if path == "/" {
		dirs = append(dirs, makeDir("a", "/a", true, plan9.DMDIR|0755))
		dirs = append(dirs, makeDir("backends", "/backends", false, 0444))
		dirs = append(dirs, makeDir("help", "/help", false, 0444))
		dirs = append(dirs, makeDir("m", "/m", true, plan9.DMDIR|0755))
		dirs = append(dirs, makeDir("p", "/p", true, plan9.DMDIR|0555))
		dirs = append(dirs, makeDir("pl", "/pl", true, plan9.DMDIR|0755))
		dirs = append(dirs, makeDir("s", "/s", true, plan9.DMDIR|0555))
		dirs = append(dirs, makeDir("sk", "/sk", true, plan9.DMDIR|0555))
		dirs = append(dirs, makeDir("t", "/t", true, plan9.DMDIR|0777))
	} else if path == "/a" {
		entries, _ := s.agentStore.List()
		for _, e := range entries {
			if !e.IsDir() {
				dirs = append(dirs, makeDir(e.Name(), "/a/"+e.Name(), false, 0666))
			}
		}
	} else if path == "/p" {
		entries, _ := s.promptStore.List()
		for _, e := range entries {
			if !e.IsDir() {
				dirs = append(dirs, makeDir(e.Name(), "/p/"+e.Name(), false, 0666))
			}
		}
	} else if path == "/m" {
		entries, _ := s.memStore.List()
		for _, e := range entries {
			if !e.IsDir() {
				d := makeDir(e.Name(), "/m/"+e.Name(), false, 0666)
				if info, err := e.Info(); err == nil {
					d.Atime = uint32(info.ModTime().Unix())
					d.Mtime = uint32(info.ModTime().Unix())
				}
				dirs = append(dirs, d)
			}
		}
	} else if path == "/pl" {
		entries, _ := s.planStore.List()
		for _, e := range entries {
			if !e.IsDir() {
				d := makeDir(e.Name(), "/pl/"+e.Name(), false, 0666)
				if info, err := e.Info(); err == nil {
					d.Atime = uint32(info.ModTime().Unix())
					d.Mtime = uint32(info.ModTime().Unix())
				}
				dirs = append(dirs, d)
			}
		}
	} else if path == "/sk" {
		entries, _ := s.skillStore.List()
		for _, e := range entries {
			mode := plan9.Perm(0666)
			if e.Name() == "idx" {
				mode = 0444
			}
			dirs = append(dirs, makeDir(e.Name(), "/sk/"+e.Name(), false, mode))
		}
	} else if path == "/t" {
		entries, _ := s.toolStore.List()
		for _, e := range entries {
			mode := plan9.Perm(0777)
			if e.Name() == "idx" {
				mode = 0444
			}
			dirs = append(dirs, makeDir(e.Name(), "/t/"+e.Name(), false, mode))
		}
	} else if path == "/s" {
		entries, _ := s.sessionStore.List()
		for _, e := range entries {
			info, _ := e.Info()
			perm := plan9.Perm(info.Mode() & 0777)
			if e.IsDir() {
				perm = plan9.DMDIR | perm
			}
			dirs = append(dirs, makeDir(e.Name(), "/s/"+e.Name(), e.IsDir(), perm))
		}
	} else {
		// Session subdirectory: /s/{sessid}
		sessID := pathBase(path)
		if store, ok := s.sessionFileStore(sessID); ok {
			entries, _ := store.List()
			for _, e := range entries {
				info, _ := e.Info()
				dirs = append(dirs, makeDir(e.Name(), path+"/"+e.Name(), false, plan9.Perm(info.Mode())))
			}
		}
	}

	// Serialize all entries to a byte slice.
	var allData []byte
	for _, d := range dirs {
		b, err := d.Bytes()
		if err != nil {
			continue
		}
		allData = append(allData, b...)
	}

	if offset >= uint64(len(allData)) {
		return nil
	}

	// Return complete entries starting at offset, up to count bytes.
	remaining := allData[offset:]
	var result []byte
	for len(remaining) >= 2 {
		// Each serialized Dir starts with a uint16 (little-endian) size of the rest.
		entrySize := int(remaining[0]) | int(remaining[1])<<8
		total := entrySize + 2
		if total > len(remaining) {
			break
		}
		if uint32(len(result)+total) > count {
			break
		}
		result = append(result, remaining[:total]...)
		remaining = remaining[total:]
	}
	return result
}

// makeStat builds a plan9.Dir for a logical path.
func (s *Server) makeStat(path string) plan9.Dir {
	base := pathBase(path)
	if path == "/" {
		base = "."
	}

	t := s.pathType(path)
	isDir := t == "dir"

	qid := plan9.Qid{Path: qidPath(path)}
	var mode plan9.Perm
	if isDir {
		qid.Type = QTDir
		if path == "/t" {
			mode = plan9.DMDIR | 0777
		} else if path == "/a" || path == "/m" || path == "/pl" {
			mode = plan9.DMDIR | 0755
		} else {
			mode = plan9.DMDIR | 0555
		}
	} else {
		switch base {
		case "ctl", "prompt", "enqueue":
			mode = 0200
		case "chat", "reply", "state", "usage", "ctxsz", "models", "mcp", "dequeue":
			mode = 0444
		case "backend", "agent", "model", "cwd":
			mode = 0666
		default:
			if path == "/backends" || path == "/help" || path == "/s/idx" {
				mode = 0444
			} else if path == "/s/new" {
				mode = 0666
			} else if strings.HasPrefix(path, "/a/") {
				mode = 0666
			} else if strings.HasPrefix(path, "/p/") {
				mode = 0444
			} else if strings.HasPrefix(path, "/m/") {
				mode = 0666
			} else if strings.HasPrefix(path, "/pl/") {
				mode = 0666
			} else if path == "/sk/idx" {
				mode = 0444
			} else if strings.HasPrefix(path, "/sk/") {
				mode = 0666
			} else if path == "/t/idx" {
				mode = 0444
			} else if strings.HasPrefix(path, "/t/") {
				mode = 0777
			} else {
				mode = 0444
			}
		}
	}

	dir := plan9.Dir{
		Qid:  qid,
		Mode: mode,
		Name: base,
		Uid:  "ollie",
		Gid:  "ollie",
		Muid: "ollie",
	}

	// For chat/reply and tailable mutable files, report actual size and
	// Qid version so polling tools (tail -f) can detect changes via stat.
	// Path format: /s/{sessid}/{file}
	if strings.HasPrefix(path, "/s/") {
		parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 3)
		if len(parts) == 3 && parts[0] == "s" {
			s.mu.RLock()
			sess := s.sessions[parts[1]]
			s.mu.RUnlock()
			if sess != nil {
				switch base {
				case "chat":
					sess.mu.RLock()
					dir.Length = uint64(len(sess.chatLog))
					dir.Qid.Vers = sess.chatVers
					sess.mu.RUnlock()
				case "reply":
					sess.mu.RLock()
					dir.Length = uint64(len(sess.core.Reply()))
					dir.Qid.Vers = sess.replyVers
					sess.mu.RUnlock()
				default:
					sess.mu.RLock()
					mf := sess.mutableVers[base]
					if mf != nil {
						dir.Qid.Vers = mf.vers
					}
					sess.mu.RUnlock()
					if mf != nil && atomic.CompareAndSwapInt32(&mf.truncated, 1, 0) {
						// Report size 0 once so tail -f detects truncation.
						dir.Length = 0
					} else if store, ok := s.sessionFileStore(parts[1]); ok {
						if info, err := store.Stat(base); err == nil {
							dir.Length = uint64(info.Size())
						}
					}
				}
			}
		}
	}

	// For memory and plan files, report real size and timestamps from the store.
	if strings.HasPrefix(path, "/m/") {
		if info, err := s.memStore.Stat(base); err == nil {
			dir.Length = uint64(info.Size())
			dir.Atime = uint32(info.ModTime().Unix())
			dir.Mtime = uint32(info.ModTime().Unix())
		}
	}
	if strings.HasPrefix(path, "/pl/") {
		if info, err := s.planStore.Stat(base); err == nil {
			dir.Length = uint64(info.Size())
			dir.Atime = uint32(info.ModTime().Unix())
			dir.Mtime = uint32(info.ModTime().Unix())
		}
	}

	return dir
}

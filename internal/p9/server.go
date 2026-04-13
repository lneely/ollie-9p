// Package p9 implements the 9P filesystem for ollie sessions.
//
// Filesystem layout:
//
//	ollie/
//	    p/                  (dir)   prompt templates
//	    pl/                 (dir)   plans
//	    s/                  (dir)   session directory
//	        new             (r/w)   read: KV template; write: create session
//	        {session-id}/           rm to kill session
//	            ctl         (write) session control: compact, clear, interrupt
//	            prompt      (write) submit a prompt to the agent
//	            enqueue     (write) queue a prompt for later execution
//	            dequeue     (read)  pop the next queued prompt
//	            chat        (read)  cumulative chat history
//	            backend     (r/w)   read/write the active backend name
//	            agent       (r/w)   read/write the active agent name
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

	"9fans.net/go/plan9"
	"ollie/pkg/agent"
	"ollie/pkg/backend"
	"ollie/pkg/config"
	"ollie/pkg/skills"
	"ollie/pkg/tools"
	"ollie/pkg/tools/execute"
	"ollie/pkg/tools/file"
	"ollie/pkg/tools/reasoning"
)

const (
	QTDir  = plan9.QTDIR
	QTFile = plan9.QTFILE
)

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
	agentsDir   string
	sessionsDir string
	promptsDir  string
	planDir     string // planning directory for /pl
}

// New creates a new Server.
func New() *Server {
	return &Server{
		sessions:    make(map[string]*session),
		agentsDir:   agent.DefaultAgentsDir(),
		sessionsDir: agent.DefaultSessionsDir(),
		promptsDir:  agent.DefaultPromptsDir(),
		planDir:     defaultPlanDir(),
	}
}

// defaultPlanDir returns the planning directory from OLLIE_PLAN_PATH or the default.
func defaultPlanDir() string {
	if p := os.Getenv("OLLIE_PLAN_PATH"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return home + "/.config/ollie/planning"
}

// Serve handles a single 9P connection.
func (s *Server) Serve(conn net.Conn) {
	defer conn.Close()
	cs := &connState{fids: make(map[uint32]*fid)}
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
	case len(parts) == 2 && parts[0] == "s" && parts[1] == "new":
		return "file"
	case len(parts) == 1 && parts[0] == "p":
		return "dir"
	case len(parts) == 1 && parts[0] == "pl":
		return "dir"
	case len(parts) == 1 && parts[0] == "sk":
		return "dir"
	case len(parts) == 1 && parts[0] == "t":
		return "dir"
	case len(parts) == 2 && parts[0] == "p":
		if _, err := os.Stat(s.promptsDir + "/" + parts[1]); err == nil {
			return "file"
		}
	case len(parts) == 2 && parts[0] == "pl":
		if _, err := os.Stat(s.planDir + "/" + parts[1]); err == nil {
			return "file"
		}
	case len(parts) == 2 && parts[0] == "sk":
		name := strings.TrimSuffix(parts[1], ".md")
		if _, err := skills.Read(name); err == nil {
			return "file"
		}
	case len(parts) == 2 && parts[0] == "t":
		if _, err := os.Stat(execute.ToolsPath() + "/" + parts[1]); err == nil {
			return "file"
		}
	case len(parts) == 2 && parts[0] == "s":
		s.mu.RLock()
		_, ok := s.sessions[parts[1]]
		s.mu.RUnlock()
		if ok {
			return "dir"
		}
	case len(parts) == 3 && parts[0] == "s":
		s.mu.RLock()
		_, ok := s.sessions[parts[1]]
		s.mu.RUnlock()
		if !ok {
			return ""
		}
		switch parts[2] {
		case "ctl", "prompt", "enqueue", "dequeue", "chat", "reply", "backend", "agent", "model", "models", "mcp", "state", "workdir", "usage", "ctxsz":
			return "file"
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
	if f.path != "/p" && f.path != "/pl" && f.path != "/sk" && f.path != "/t" {
		return errFcall(fc, "create not supported")
	}
	if fc.Perm&plan9.DMDIR != 0 {
		return errFcall(fc, "mkdir not supported")
	}

	newPath := pathJoin(f.path, fc.Name)

	// Actually create the file on disk so that even a no-write create
	// (e.g. touch) produces a real file.
	switch f.path {
	case "/p":
		os.MkdirAll(s.promptsDir, 0755) //nolint:errcheck
		if err := os.WriteFile(s.promptsDir+"/"+fc.Name, nil, 0644); err != nil {
			return errFcall(fc, err.Error())
		}
	case "/pl":
		os.MkdirAll(s.planDir, 0755) //nolint:errcheck
		if err := os.WriteFile(s.planDir+"/"+fc.Name, nil, 0644); err != nil {
			return errFcall(fc, err.Error())
		}
	case "/t":
		dir := execute.ToolsPath()
		os.MkdirAll(dir, 0755) //nolint:errcheck
		if err := os.WriteFile(dir+"/"+fc.Name, nil, 0755); err != nil {
			return errFcall(fc, err.Error())
		}
	case "/sk":
		name := strings.TrimSuffix(fc.Name, ".md")
		dir := skills.Dirs()[0] + "/" + name
		if err := os.MkdirAll(dir, 0755); err != nil {
			return errFcall(fc, err.Error())
		}
		if err := os.WriteFile(dir+"/SKILL.md", nil, 0644); err != nil {
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

	// chat, reply, and dequeue are served from session state.
	switch pathBase(path) {
	case "chat":
		return s.readChat(fc, path)
	case "reply":
		return s.readReply(fc, path)
	case "dequeue":
		return s.readDequeue(fc, path)
	}

	// s/new returns the session creation template.
	if path == "/s/new" {
		content := []byte("workdir=\nbackend=\nmodel=\nagent=\n")
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

	// Prompt files are served from disk.
	if strings.HasPrefix(path, "/p/") {
		content, err := os.ReadFile(s.promptsDir + "/" + pathBase(path))
		if err != nil {
			return errFcall(fc, err.Error())
		}
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

	// Plan files are served from disk.
	if strings.HasPrefix(path, "/pl/") {
		content, err := os.ReadFile(s.planDir + "/" + pathBase(path))
		if err != nil {
			return errFcall(fc, err.Error())
		}
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

	// Skill files are served via pkg/skills (reads from disk each time).
	if strings.HasPrefix(path, "/sk/") {
		name := strings.TrimSuffix(pathBase(path), ".md")
		content, err := skills.Read(name)
		if err != nil {
			return errFcall(fc, err.Error())
		}
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

	// Tool files are served from the tools path.
	if strings.HasPrefix(path, "/t/") {
		content, err := os.ReadFile(execute.ToolsPath() + "/" + pathBase(path))
		if err != nil {
			return errFcall(fc, err.Error())
		}
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

	content := s.readFile(path)
	var data []byte
	off := int(fc.Offset)
	if off < len(content) {
		end := off + int(fc.Count)
		if end > len(content) {
			end = len(content)
		}
		data = []byte(content[off:end])
	}
	return &plan9.Fcall{Type: plan9.Rread, Tag: fc.Tag, Count: uint32(len(data)), Data: data}
}

// readChat serves bytes from the session's chatLog at the requested offset.
// Returns 0 bytes when offset is at or past the current end (normal EOF);
// tail -f detects growth via stat and re-reads from its last position.
func (s *Server) readChat(fc *plan9.Fcall, path string) *plan9.Fcall {
	parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 3)
	if len(parts) != 3 || parts[0] != "s" {
		return &plan9.Fcall{Type: plan9.Rread, Tag: fc.Tag, Count: 0}
	}
	s.mu.RLock()
	sess := s.sessions[parts[1]]
	s.mu.RUnlock()
	if sess == nil {
		return &plan9.Fcall{Type: plan9.Rread, Tag: fc.Tag, Count: 0}
	}

	sess.mu.RLock()
	log := sess.chatLog
	sess.mu.RUnlock()

	var data []byte
	off := int(fc.Offset)
	if off < len(log) {
		end := off + int(fc.Count)
		if end > len(log) {
			end = len(log)
		}
		data = log[off:end]
	}
	return &plan9.Fcall{Type: plan9.Rread, Tag: fc.Tag, Count: uint32(len(data)), Data: data}
}

// readDequeue pops the next queued prompt from the session's FIFO.
// Only pops on offset==0; a non-zero offset signals the EOF read that
// follows a successful read and must not consume another item.
func (s *Server) readDequeue(fc *plan9.Fcall, path string) *plan9.Fcall {
	parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 3)
	if len(parts) != 3 || parts[0] != "s" {
		return &plan9.Fcall{Type: plan9.Rread, Tag: fc.Tag, Count: 0}
	}
	s.mu.RLock()
	sess := s.sessions[parts[1]]
	s.mu.RUnlock()
	if sess == nil {
		return &plan9.Fcall{Type: plan9.Rread, Tag: fc.Tag, Count: 0}
	}
	// Non-zero offset is the trailing EOF read; don't pop another item.
	if fc.Offset > 0 {
		return &plan9.Fcall{Type: plan9.Rread, Tag: fc.Tag, Count: 0}
	}
	item, ok := sess.core.PopQueue()
	if !ok {
		return &plan9.Fcall{Type: plan9.Rread, Tag: fc.Tag, Count: 0}
	}
	data := []byte(item)
	if uint32(len(data)) > fc.Count {
		data = data[:fc.Count]
	}
	return &plan9.Fcall{Type: plan9.Rread, Tag: fc.Tag, Count: uint32(len(data)), Data: data}
}

// readReply serves bytes from the session's reply buffer at the requested offset.
func (s *Server) readReply(fc *plan9.Fcall, path string) *plan9.Fcall {
	parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 3)
	if len(parts) != 3 || parts[0] != "s" {
		return &plan9.Fcall{Type: plan9.Rread, Tag: fc.Tag, Count: 0}
	}
	s.mu.RLock()
	sess := s.sessions[parts[1]]
	s.mu.RUnlock()
	if sess == nil {
		return &plan9.Fcall{Type: plan9.Rread, Tag: fc.Tag, Count: 0}
	}

	buf := []byte(sess.core.Reply())

	var data []byte
	off := int(fc.Offset)
	if off < len(buf) {
		end := off + int(fc.Count)
		if end > len(buf) {
			end = len(buf)
		}
		data = buf[off:end]
	}
	return &plan9.Fcall{Type: plan9.Rread, Tag: fc.Tag, Count: uint32(len(data)), Data: data}
}

// readFile returns the text content for a readable session file.
func (s *Server) readFile(path string) string {
	parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 3)
	if len(parts) != 3 || parts[0] != "s" {
		return ""
	}
	sessID, fileName := parts[1], parts[2]

	s.mu.RLock()
	sess := s.sessions[sessID]
	s.mu.RUnlock()
	if sess == nil {
		return ""
	}

	sess.mu.RLock()
	defer sess.mu.RUnlock()
	switch fileName {
	case "backend":
		return sess.core.BackendName() + "\n"
	case "agent":
		return sess.core.AgentName() + "\n"
	case "model":
		return sess.core.ModelName() + "\n"
	case "state":
		return sess.core.State() + "\n"
	case "workdir":
		return sess.core.WorkDir() + "\n"
	case "usage":
		return sess.core.Usage() + "\n"
	case "ctxsz":
		return sess.core.CtxSz() + "\n"
	case "models":
		return sess.core.ListModels() + "\n"
	case "mcp":
		return sess.core.ListServers() + "\n"
	}
	return ""
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

	// Only /pl files support rename via wstat.
	if !strings.HasPrefix(f.path, "/pl/") {
		return &plan9.Fcall{Type: plan9.Rwstat, Tag: fc.Tag}
	}

	// Parse the new Dir from the stat bytes.
	newDir, err := plan9.UnmarshalDir(fc.Stat)
	if err != nil {
		// Some clients send minimal wstat (e.g. truncate); accept silently.
		return &plan9.Fcall{Type: plan9.Rwstat, Tag: fc.Tag}
	}

	// If the name changed, rename the file on disk.
	oldName := pathBase(f.path)
	if newDir.Name != "" && newDir.Name != oldName {
		oldPath := s.planDir + "/" + oldName
		newPath := s.planDir + "/" + newDir.Name
		if err := os.Rename(oldPath, newPath); err != nil {
			return errFcall(fc, err.Error())
		}
		cs.mu.Lock()
		f.path = "/pl/" + newDir.Name
		f.qid.Path = qidPath(f.path)
		cs.mu.Unlock()
	}

	return &plan9.Fcall{Type: plan9.Rwstat, Tag: fc.Tag}
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
	case strings.HasPrefix(path, "/p/"):
		err = os.Remove(s.promptsDir + "/" + pathBase(path))
	case strings.HasPrefix(path, "/sk/"):
		name := strings.TrimSuffix(pathBase(path), ".md")
		for _, m := range skills.List() {
			if m.Name == name {
				err = os.RemoveAll(m.Dir)
				break
			}
		}
	case strings.HasPrefix(path, "/t/"):
		err = os.Remove(execute.ToolsPath() + "/" + pathBase(path))
	case strings.HasPrefix(path, "/s/") && path != "/s/new":
		s.killSession(pathBase(path))
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
		return s.handleNewSession(input)
	}

	// Prompt file writes go directly to disk.
	if strings.HasPrefix(path, "/p/") {
		return os.WriteFile(s.promptsDir+"/"+pathBase(path), []byte(input), 0644)
	}

	// Plan file writes go directly to disk.
	if strings.HasPrefix(path, "/pl/") {
		os.MkdirAll(s.planDir, 0755) //nolint:errcheck
		return os.WriteFile(s.planDir+"/"+pathBase(path), []byte(input), 0644)
	}

	// Skill file writes: create skill directory and write SKILL.md.
	if strings.HasPrefix(path, "/sk/") {
		name := strings.TrimSuffix(pathBase(path), ".md")
		dir := ""
		for _, m := range skills.List() {
			if m.Name == name {
				dir = m.Dir
				break
			}
		}
		if dir == "" {
			dir = skills.Dirs()[0] + "/" + name
		}
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
		return os.WriteFile(dir+"/SKILL.md", []byte(input), 0644)
	}

	// Tool file writes go directly to disk.
	if strings.HasPrefix(path, "/t/") {
		return os.WriteFile(execute.ToolsPath()+"/"+pathBase(path), []byte(input), 0755)
	}

	// Path format: /s/{sessid}/{file}
	parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 3)
	if len(parts) != 3 || parts[0] != "s" {
		return nil
	}
	sessID, fileName := parts[1], parts[2]

	s.mu.RLock()
	sess := s.sessions[sessID]
	s.mu.RUnlock()
	if sess == nil {
		return fmt.Errorf("session not found: %s", sessID)
	}

	// assistantStarted tracks whether we've emitted the "assistant: " label
	// for the current turn. Resets at turn boundaries so each response gets
	// exactly one label regardless of how many streaming chunks arrive.
	assistantStarted := false
	publish := func(ev agent.Event) {
		switch ev.Role {
		case "call", "tool", "newline":
			assistantStarted = false
		case "assistant":
			if !assistantStarted {
				sess.appendChat([]byte("assistant: "))
				assistantStarted = true
			}
		}
		sess.appendChat(formatEvent(ev))
	}

	switch fileName {
	case "prompt":
		sess.core.Submit(sess.ctx, input, publish)
		sess.mu.Lock()
		sess.replyVers++
		sess.mu.Unlock()
		return nil

	case "enqueue":
		sess.core.Queue(input)
		return nil

	case "ctl":
		switch input {
		case "stop":
			sess.core.Interrupt(agent.ErrInterrupted)
		default:
			sess.core.Submit(sess.ctx, "/"+input, publish)
		}
		return nil

	case "backend":
		if sess.core.IsRunning() {
			return fmt.Errorf("cannot switch backend while agent is running")
		}
		sess.core.Submit(sess.ctx, "/backend "+input, publish)
		return nil

	case "agent":
		if sess.core.IsRunning() {
			return fmt.Errorf("cannot switch agent while agent is running")
		}
		sess.core.Submit(sess.ctx, "/agent "+input, publish)
		return nil

	case "model":
		if input == "" {
			return fmt.Errorf("empty model name")
		}
		if sess.core.IsRunning() {
			return fmt.Errorf("cannot switch model while agent is running")
		}
		sess.core.Submit(sess.ctx, "/model "+input, publish)
		return nil

	case "workdir":
		return sess.core.SetWorkDir(input)
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
// Recognised keys: backend, model, agent, workdir. workdir is required.
// Example: new workdir=/home/lkn/src/myproject backend=ollama model=qwen3:8b agent=myagent
func (s *Server) createSession(args []string) error {
	backendOverride := ""
	modelOverride := ""
	agentName := "default"
	workdir := ""

	for _, arg := range args {
		k, v, ok := strings.Cut(arg, "=")
		if !ok || v == "" {
			return fmt.Errorf("invalid option %q (expected key=value)", arg)
		}
		switch k {
		case "backend":
			backendOverride = v
		case "model":
			modelOverride = v
		case "agent":
			agentName = v
		case "workdir":
			workdir = v
		default:
			return fmt.Errorf("unknown option %q (valid: backend, model, agent, workdir)", k)
		}
	}

	if workdir == "" {
		return fmt.Errorf("workdir is required (e.g. new workdir=/path/to/project)")
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
		"execute":   execute.Decl(workdir),
		"file":      file.Decl(workdir),
		"reasoning": reasoning.Decl(),
	})

	sessID := agent.NewSessionID()
	fallback := &filePlanBackend{dir: s.planDir}
	env := agent.BuildAgentEnv(cfg, newDisp(), workdir, agent.WithFallbackPlanBackend(fallback))

	// Inject per-session env vars into the execute server.
	if srv, ok := env.Dispatcher().GetServer("execute"); ok {
		if es, ok := srv.(tools.EnvSetter); ok {
			es.SetEnv("OLLIE_SESSION_ID", sessID)
			es.SetEnv("OLLIE_PLAN_PATH", s.planDir)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	core := agent.NewAgentCore(agent.AgentCoreConfig{
		Backend:       be,
		AgentName:     agentName,
		AgentsDir:     s.agentsDir,
		SessionsDir:   s.sessionsDir,
		SessionID:     sessID,
		WorkDir:       workdir,
		Env:           env,
		NewDispatcher: newDisp,
	})

	sess := &session{
		id:     sessID,
		core:   core,
		ctx:    ctx,
		cancel: cancel,
	}

	s.mu.Lock()
	s.sessions[sessID] = sess
	s.mu.Unlock()

	fmt.Fprintf(os.Stderr, "olliesrv: new session %s (backend=%s model=%s agent=%s)\n",
		sessID, core.BackendName(), core.ModelName(), core.AgentName())
	return nil
}

func (s *Server) killSession(id string) {
	s.mu.Lock()
	sess := s.sessions[id]
	delete(s.sessions, id)
	s.mu.Unlock()
	if sess != nil {
		sess.cancel()
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
		dirs = append(dirs, makeDir("p", "/p", true, plan9.DMDIR|0755))
		dirs = append(dirs, makeDir("pl", "/pl", true, plan9.DMDIR|0755))
		dirs = append(dirs, makeDir("s", "/s", true, plan9.DMDIR|0555))
		dirs = append(dirs, makeDir("sk", "/sk", true, plan9.DMDIR|0555))
		dirs = append(dirs, makeDir("t", "/t", true, plan9.DMDIR|0777))
	} else if path == "/p" {
		entries, _ := os.ReadDir(s.promptsDir)
		for _, e := range entries {
			if !e.IsDir() {
				dirs = append(dirs, makeDir(e.Name(), "/p/"+e.Name(), false, 0666))
			}
		}
	} else if path == "/pl" {
		entries, _ := os.ReadDir(s.planDir)
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
		for _, m := range skills.List() {
			name := m.Name + ".md"
			dirs = append(dirs, makeDir(name, "/sk/"+name, false, 0666))
		}
	} else if path == "/t" {
		entries, _ := os.ReadDir(execute.ToolsPath())
		for _, e := range entries {
			if !e.IsDir() {
				dirs = append(dirs, makeDir(e.Name(), "/t/"+e.Name(), false, 0777))
			}
		}
	} else if path == "/s" {
		dirs = append(dirs, makeDir("new", "/s/new", false, 0666))
		s.mu.RLock()
		for id := range s.sessions {
			dirs = append(dirs, makeDir(id, "/s/"+id, true, plan9.DMDIR|0555))
		}
		s.mu.RUnlock()
	} else {
		// Session directory: path is /s/{sessid}
		sessPath := path
		type entry struct {
			name string
			mode plan9.Perm
		}
		files := []entry{
			{"ctl", 0200},
			{"prompt", 0200},
			{"enqueue", 0200},
			{"dequeue", 0444},
			{"chat", 0444},
			{"reply", 0444},
			{"state", 0444},
			{"backend", 0666},
			{"agent", 0666},
			{"model", 0666},
			{"workdir", 0666},
			{"usage", 0444},
			{"ctxsz", 0444},
			{"models", 0444},
			{"mcp", 0444},
		}
		for _, e := range files {
			dirs = append(dirs, makeDir(e.name, sessPath+"/"+e.name, false, e.mode))
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
		} else if path == "/p" || path == "/pl" {
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
		case "backend", "agent", "model", "workdir":
			mode = 0666
		default:
			if path == "/s/new" {
				mode = 0666
			} else if strings.HasPrefix(path, "/p/") {
				mode = 0666
			} else if strings.HasPrefix(path, "/pl/") {
				mode = 0666
			} else if strings.HasPrefix(path, "/sk/") {
				mode = 0666
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

	// For chat/reply files, report actual size and Qid version so that
	// polling tools (tail -f) can detect growth via stat.
	// Path format: /s/{sessid}/{file}
	if base == "chat" || base == "reply" {
		parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 3)
		if len(parts) == 3 && parts[0] == "s" {
			s.mu.RLock()
			sess := s.sessions[parts[1]]
			s.mu.RUnlock()
			if sess != nil {
				if base == "chat" {
					sess.mu.RLock()
					dir.Length = uint64(len(sess.chatLog))
					dir.Qid.Vers = sess.chatVers
					sess.mu.RUnlock()
				} else {
					dir.Length = uint64(len(sess.core.Reply()))
					sess.mu.RLock()
					dir.Qid.Vers = sess.replyVers
					sess.mu.RUnlock()
				}
			}
		}
	}

	// For plan files, report real size and timestamps from disk.
	if strings.HasPrefix(path, "/pl/") {
		if info, err := os.Stat(s.planDir + "/" + base); err == nil {
			dir.Length = uint64(info.Size())
			dir.Atime = uint32(info.ModTime().Unix())
			dir.Mtime = uint32(info.ModTime().Unix())
		}
	}

	return dir
}

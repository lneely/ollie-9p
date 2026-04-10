// Package p9 implements the 9P filesystem for ollie sessions.
//
// Filesystem layout:
//
//	ollie/
//	    ctl                 (write) create/destroy sessions
//	                                "new [backend] [agent]" creates a session
//	                                "kill <session-id>" destroys a session
//	    s/                  (dir)   session directory; one entry per active session,
//	                                sorted lexicographically by session ID (= creation
//	                                order, since IDs are unix-nanosecond timestamps).
//	        {session-id}/
//	            ctl         (write) session control: compact, clear, interrupt
//	            prompt      (write) submit a prompt to the agent
//	            chat        (read)  cumulative chat history; grows as conversation
//	                                progresses — use cat for a snapshot, tail -f
//	                                to follow new output.
//	            backend     (r/w)   read/write the active backend name
//	            agent       (r/w)   read/write the active agent name
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
	mu          sync.RWMutex
	id          string
	core        agent.Core
	backendName string
	agentName   string
	ctx         context.Context
	cancel      context.CancelFunc
	// chatLog is an append-only record of the conversation. Reads are served
	// directly from this buffer by offset, so tail -f works via polling.
	chatLog  []byte
	chatVers uint32 // incremented on each append; used as Qid.Vers
	// state is the current agent state: "idle", "thinking", "calling: <tool>"
	state     string
	modelName string
	workdir   string
	// reply holds only the assistant text from the most recently completed
	// turn. Cleared when a new prompt is submitted. Read-only; 0444.
	reply     []byte
	replyVers uint32
}

func (sess *session) setState(s string) {
	sess.mu.Lock()
	sess.state = s
	sess.mu.Unlock()
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
}

// New creates a new Server.
func New() *Server {
	return &Server{
		sessions:    make(map[string]*session),
		agentsDir:   agent.DefaultAgentsDir(),
		sessionsDir: agent.DefaultSessionsDir(),
		promptsDir:  agent.DefaultPromptsDir(),
	}
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
		return errFcall(fc, "create not supported")
	case plan9.Tread:
		return s.read(cs, fc)
	case plan9.Twrite:
		return s.write(cs, fc)
	case plan9.Tstat:
		return s.stat(cs, fc)
	case plan9.Twstat:
		// Accept and ignore wstat; 9pfuse sends Twstat for O_TRUNC on open files.
		return &plan9.Fcall{Type: plan9.Rwstat, Tag: fc.Tag}
	case plan9.Tflush:
		// No blocking reads to cancel; always succeed.
		return &plan9.Fcall{Type: plan9.Rflush, Tag: fc.Tag}
	case plan9.Tclunk:
		return s.clunk(cs, fc)
	case plan9.Tremove:
		return errFcall(fc, "remove not supported")
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
	case len(parts) == 1 && parts[0] == "ctl":
		return "file"
	case len(parts) == 1 && parts[0] == "s":
		return "dir"
	case len(parts) == 1 && parts[0] == "p":
		return "dir"
	case len(parts) == 1 && parts[0] == "sk":
		return "dir"
	case len(parts) == 1 && parts[0] == "t":
		return "dir"
	case len(parts) == 2 && parts[0] == "p":
		if _, err := os.Stat(s.promptsDir + "/" + parts[1]); err == nil {
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
		case "ctl", "prompt", "prompt.fifo", "chat", "reply", "backend", "agent", "model", "models", "state", "workdir", "usage":
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

	// chat, reply, and prompt.fifo are served from session state.
	switch pathBase(path) {
	case "chat":
		return s.readChat(fc, path)
	case "reply":
		return s.readReply(fc, path)
	case "prompt.fifo":
		return s.readPromptFIFO(fc, path)
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

// readPromptFIFO pops the next queued prompt from the session's FIFO.
func (s *Server) readPromptFIFO(fc *plan9.Fcall, path string) *plan9.Fcall {
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
	item, ok := sess.core.PopQueue()
	if !ok {
		return &plan9.Fcall{Type: plan9.Rread, Tag: fc.Tag, Count: 0}
	}
	data := []byte(item)
	if int(fc.Offset) >= len(data) {
		return &plan9.Fcall{Type: plan9.Rread, Tag: fc.Tag, Count: 0}
	}
	data = data[fc.Offset:]
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

	sess.mu.RLock()
	buf := sess.reply
	sess.mu.RUnlock()

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
		return sess.backendName + "\n"
	case "agent":
		return sess.agentName + "\n"
	case "model":
		return sess.modelName + "\n"
	case "state":
		return sess.state + "\n"
	case "workdir":
		return sess.workdir + "\n"
	case "usage":
		return sess.core.Usage() + "\n"
	case "models":
		return sess.core.ListModels() + "\n"
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

func (s *Server) clunk(cs *connState, fc *plan9.Fcall) *plan9.Fcall {
	cs.mu.Lock()
	f, ok := cs.fids[fc.Fid]
	if ok {
		if len(f.writeBuf) > 0 {
			path := f.path
			data := make([]byte, len(f.writeBuf))
			copy(data, f.writeBuf)
			go s.handleWrite(path, strings.TrimSpace(string(data)))
		}
		delete(cs.fids, fc.Fid)
	}
	cs.mu.Unlock()
	return &plan9.Fcall{Type: plan9.Rclunk, Tag: fc.Tag}
}

// handleWrite processes a fully-assembled write payload for the given path.
// Called asynchronously from clunk.
func (s *Server) handleWrite(path, input string) {
	if input == "" {
		return
	}

	if path == "/ctl" {
		s.handleRootCtl(input)
		return
	}

	// Prompt file writes go directly to disk.
	if strings.HasPrefix(path, "/p/") {
		if err := os.WriteFile(s.promptsDir+"/"+pathBase(path), []byte(input), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "olliesrv: write prompt: %v\n", err)
		}
		return
	}

	// Tool file writes go directly to disk.
	if strings.HasPrefix(path, "/t/") {
		if err := os.WriteFile(execute.ToolsPath()+"/"+pathBase(path), []byte(input), 0755); err != nil {
			fmt.Fprintf(os.Stderr, "olliesrv: write tool: %v\n", err)
		}
		return
	}

	// Path format: /s/{sessid}/{file}
	parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 3)
	if len(parts) != 3 || parts[0] != "s" {
		return
	}
	sessID, fileName := parts[1], parts[2]

	s.mu.RLock()
	sess := s.sessions[sessID]
	s.mu.RUnlock()
	if sess == nil {
		return
	}

	// assistantStarted tracks whether we've emitted the "assistant: " label
	// for the current turn. Resets at turn boundaries so each response gets
	// exactly one label regardless of how many streaming chunks arrive.
	assistantStarted := false
	publish := func(ev agent.Event) {
		switch ev.Role {
		case "call":
			sess.setState("calling: " + ev.Name)
			assistantStarted = false
		case "tool":
			sess.setState("thinking")
			assistantStarted = false
		case "newline":
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
		// If the agent is busy, inject mid-turn instead of starting a new turn.
		if sess.core.IsRunning() {
			sess.core.Inject(input)
			sess.appendChat([]byte("user (interrupt): " + input + "\n"))
			return
		}
		sess.appendChat([]byte("user: " + input + "\n"))
		// Clear reply before submission — documented side-effect.
		sess.mu.Lock()
		sess.reply = nil
		sess.replyVers++
		sess.mu.Unlock()
		sess.setState("thinking")
		var replyBuf []byte
		sess.core.Submit(sess.ctx, input, func(ev agent.Event) {
			if ev.Role == "assistant" {
				replyBuf = append(replyBuf, []byte(ev.Content)...)
			}
			publish(ev)
		})
		// Atomically publish reply on turn completion.
		sess.mu.Lock()
		sess.reply = replyBuf
		sess.replyVers++
		sess.mu.Unlock()
		sess.setState("idle")

	case "prompt.fifo":
		sess.core.Queue(input)

	case "ctl":
		switch {
		case input == "stop" || input == "interrupt":
			sess.core.Interrupt(agent.ErrInterrupted)
		case strings.HasPrefix(input, "/"):
			sess.core.Submit(sess.ctx, input, publish)
		default:
			fmt.Fprintf(os.Stderr, "olliesrv: session ctl: unknown command %q (use stop, interrupt, or /slashcmd)\n", input)
		}

	case "backend":
		sess.mu.Lock()
		old := sess.backendName
		sess.backendName = input
		sess.mu.Unlock()
		sess.core.Submit(sess.ctx, "/backend "+input, func(ev agent.Event) {
			if ev.Role == "error" {
				sess.mu.Lock()
				sess.backendName = old
				sess.mu.Unlock()
			}
			publish(ev)
		})

	case "agent":
		sess.mu.Lock()
		old := sess.agentName
		sess.agentName = input
		sess.mu.Unlock()
		sess.core.Submit(sess.ctx, "/agent "+input, func(ev agent.Event) {
			if ev.Role == "error" {
				sess.mu.Lock()
				sess.agentName = old
				sess.mu.Unlock()
			}
			publish(ev)
		})

	case "model":
		if input == "" {
			fmt.Fprintf(os.Stderr, "olliesrv: model: empty model name\n")
			return
		}
		sess.mu.Lock()
		old := sess.modelName
		sess.modelName = input
		sess.mu.Unlock()
		sess.core.Submit(sess.ctx, "/model "+input, func(ev agent.Event) {
			if ev.Role == "error" {
				sess.mu.Lock()
				sess.modelName = old
				sess.mu.Unlock()
			}
			publish(ev)
		})

	case "workdir":
		sess.mu.Lock()
		sess.workdir = input
		sess.mu.Unlock()
	}
}

func (s *Server) handleRootCtl(input string) {
	parts := strings.Fields(input)
	if len(parts) == 0 {
		return
	}
	switch parts[0] {
	case "new":
		if err := s.createSession(parts[1:]); err != nil {
			fmt.Fprintf(os.Stderr, "olliesrv: new session: %v\n", err)
		}
	case "kill":
		if len(parts) < 2 {
			fmt.Fprintf(os.Stderr, "olliesrv: ctl: kill requires a session id\n")
			return
		}
		s.killSession(parts[1])
	default:
		fmt.Fprintf(os.Stderr, "olliesrv: ctl: unknown command %q (use: new [backend] [agent], kill <id>)\n", parts[0])
	}
}

// createSession parses key=value options and starts a new agent session.
// Recognised keys: backend, model, agent, workdir. Unknown keys are rejected.
// Example: new backend=ollama model=qwen3:8b agent=myagent workdir=/home/lkn/src/myproject
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
		"file":      file.Decl,
		"reasoning": reasoning.Decl(),
	})

	env := agent.BuildAgentEnv(cfg, newDisp(), workdir)
	sessID := agent.NewSessionID()

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
		id:          sessID,
		core:        core,
		backendName: be.Name(),
		agentName:   agentName,
		modelName:   be.Model(),
		workdir:     workdir,
		ctx:         ctx,
		cancel:      cancel,
		state:       "idle",
	}

	s.mu.Lock()
	s.sessions[sessID] = sess
	s.mu.Unlock()

	fmt.Fprintf(os.Stderr, "olliesrv: new session %s (backend=%s model=%s agent=%s)\n",
		sessID, sess.backendName, sess.modelName, agentName)
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
	case "assistant":
		return []byte(ev.Content)
	case "call":
		args := squashWhitespace(ev.Content)
		if len(args) > 500 {
			args = args[:500] + "..."
		}
		return []byte("-> " + ev.Name + "(" + args + ")\n")
	case "tool":
		s := strings.TrimRight(ev.Content, "\n")
		if len(s) > 500 {
			s = s[:500] + "..."
		}
		return []byte("= " + s + "\n")
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
		dirs = append(dirs, makeDir("ctl", "/ctl", false, 0200))
		dirs = append(dirs, makeDir("p", "/p", true, plan9.DMDIR|0555))
		dirs = append(dirs, makeDir("s", "/s", true, plan9.DMDIR|0555))
		dirs = append(dirs, makeDir("sk", "/sk", true, plan9.DMDIR|0555))
		dirs = append(dirs, makeDir("t", "/t", true, plan9.DMDIR|0555))
	} else if path == "/p" {
		entries, _ := os.ReadDir(s.promptsDir)
		for _, e := range entries {
			if !e.IsDir() {
				dirs = append(dirs, makeDir(e.Name(), "/p/"+e.Name(), false, 0666))
			}
		}
	} else if path == "/sk" {
		for _, m := range skills.List() {
			name := m.Name + ".md"
			dirs = append(dirs, makeDir(name, "/sk/"+name, false, 0444))
		}
	} else if path == "/t" {
		entries, _ := os.ReadDir(execute.ToolsPath())
		for _, e := range entries {
			if !e.IsDir() {
				dirs = append(dirs, makeDir(e.Name(), "/t/"+e.Name(), false, 0666))
			}
		}
	} else if path == "/s" {
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
			{"prompt.fifo", 0666},
			{"chat", 0444},
			{"reply", 0444},
			{"state", 0444},
			{"backend", 0666},
			{"agent", 0666},
			{"model", 0666},
			{"workdir", 0666},
			{"usage", 0444},
			{"models", 0444},
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
		mode = plan9.DMDIR | 0555
	} else {
		switch base {
		case "ctl", "prompt":
			mode = 0200
		case "prompt.fifo":
			mode = 0666
		case "chat", "reply", "state", "usage", "models":
			mode = 0444
		case "backend", "agent", "model", "workdir":
			mode = 0666
		default:
			if strings.HasPrefix(path, "/p/") {
				mode = 0666
			} else if strings.HasPrefix(path, "/sk/") {
				mode = 0444
			} else if strings.HasPrefix(path, "/t/") {
				mode = 0666
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
				sess.mu.RLock()
				if base == "chat" {
					dir.Length = uint64(len(sess.chatLog))
					dir.Qid.Vers = sess.chatVers
				} else {
					dir.Length = uint64(len(sess.reply))
					dir.Qid.Vers = sess.replyVers
				}
				sess.mu.RUnlock()
			}
		}
	}

	return dir
}

// 9P filesystem server for ollie sessions.
// See doc comment below for filesystem layout.
package main

import (
	"context"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"ollie/pkg/agent"
	"ollie/pkg/backend"
	olog "ollie/pkg/log"
	"ollie/pkg/paths"
	"ollie/pkg/store"
	"ollie/pkg/tools/execute"

	"9fans.net/go/plan9"
)

// Re-export core store types so existing 9p code compiles unchanged.
type (
	StoreEntry    = store.StoreEntry
	Store         = store.Store
	RunnableStore = store.RunnableStore
	FlatDirStore  = store.Store
	Session       = store.Session
	SessionStore  = store.SessionStore

	syntheticFileInfo = store.SyntheticFileInfo
)

func NewFlatDirStore(dir string, perm os.FileMode) FlatDirStore {
	return store.NewFlatDir(dir, perm)
}

func NewSkillStore() Store {
	return store.NewSkillStore()
}

func syntheticEntry(name string, mode os.FileMode) os.DirEntry {
	return store.FileEntry(name, mode)
}

func syntheticDirEntry(name string, mode os.FileMode) os.DirEntry {
	return store.DirEntry(name, mode)
}

// --- util ---

// UtilStore is a FlatDirStore backed by the scripts/u/ directory.
type UtilStore struct {
	FlatDirStore
}

func NewUtilStore() *UtilStore {
	return &UtilStore{FlatDirStore: NewFlatDirStore(paths.CfgDir()+"/scripts/u", 0555)}
}

func (s *UtilStore) Stat(name string) (os.FileInfo, error) {
	return s.FlatDirStore.Stat(name)
}

func (s *UtilStore) List() ([]os.DirEntry, error) {
	return s.FlatDirStore.List()
}

func (s *UtilStore) Open(name string) (StoreEntry, error) {
	return s.FlatDirStore.Open(name)
}

// --- exec ---

// ExecStore is a read-only FlatDirStore backed by the scripts/x/ directory.
type ExecStore struct {
	FlatDirStore
}

func NewExecStore() *ExecStore {
	return &ExecStore{FlatDirStore: NewFlatDirStore(execute.PluginsPath(), 0555)}
}

func (s *ExecStore) Stat(name string) (os.FileInfo, error) {
	return s.FlatDirStore.Stat(name)
}

func (s *ExecStore) List() ([]os.DirEntry, error) {
	return s.FlatDirStore.List()
}

func (s *ExecStore) Open(name string) (StoreEntry, error) {
	return s.FlatDirStore.Open(name)
}

// --- tools ---

// ToolStore is a BlobStore backed by the tools directory.
// It extends FlatDirStore with a synthetic "idx" entry that lists all tools
// with their descriptions and argument signatures.
type ToolStore struct {
	FlatDirStore
}

func NewToolStore() *ToolStore {
	return &ToolStore{FlatDirStore: NewFlatDirStore(execute.ToolsPath(), 0755)}
}

func (s *ToolStore) Stat(name string) (os.FileInfo, error) {
	if name == "idx" {
		return &syntheticFileInfo{Name_: "idx", Mode_: 0444}, nil
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

func (s *ToolStore) Open(name string) (StoreEntry, error) {
	if name == "idx" {
		return &store.EntryConfig{
			StatFn: func() (os.FileInfo, error) { return &syntheticFileInfo{Name_: "idx", Mode_: 0444}, nil },
			ReadFn: func() ([]byte, error) { return s.index() },
			WriteFn: func([]byte) error { return fmt.Errorf("idx: read-only") },
			BlockingReadFn: func(context.Context, string) ([]byte, error) { return nil, fmt.Errorf("blocking read not supported") },
		}, nil
	}
	return s.FlatDirStore.Open(name)
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
		data, err := storeRead(s.FlatDirStore, e.Name())
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

const (
	QTDir  = plan9.QTDIR
	QTFile = plan9.QTFILE
)

// fid tracks per-descriptor state for a single 9P connection.
type fid struct {
	path     string
	qid      plan9.Qid
	mode     uint8
	writeBuf []byte
	waitBase string // for *wait files: value snapshotted at open time
}

// connState tracks all open fids for a single 9P connection.
type connState struct {
	mu      sync.RWMutex
	fids    map[uint32]*fid
	ctx     context.Context
	cancel  context.CancelFunc
	pending map[uint16]context.CancelFunc // in-flight request cancels, keyed by tag
}

// Server is the 9P server for ollie sessions.
type Server struct {
	mu              sync.RWMutex
	conns           []*connState
	log             *olog.Logger
	sink            *olog.Sink
	agentsDir       string // kept for session creation wiring
	agentStore      Store
	promptStore     Store
	memStore        Store
	toolStore       Store
	utilStore       Store
	pluginStore     Store
	skillStore      Store
	sessionStore    *SessionStore
	transcriptStore Store
	tmpStore        Store
}

// New creates a new Server.
func New(sink *olog.Sink) *Server {
	memDir := defaultMemDir()
	os.MkdirAll(memDir, 0755) //nolint:errcheck
	transcriptDir := defaultTranscriptDir()
	os.MkdirAll(transcriptDir, 0755) //nolint:errcheck
	tmpDir := defaultTmpDir()
	os.MkdirAll(tmpDir, 0755) //nolint:errcheck
	agentsDir := paths.CfgDir() + "/agents"
	sessionsDir := paths.CfgDir() + "/sessions"
	s := &Server{
		log:             sink.Logger("9p", olog.LevelDebug),
		sink:            sink,
		agentsDir:       agentsDir,
		agentStore:      NewFlatDirStore(agentsDir, 0644),
		promptStore:     NewFlatDirStore(agent.DefaultPromptsDir(), 0444),
		memStore:        NewFlatDirStore(memDir, 0644),
		toolStore:       NewToolStore(),
		utilStore:       NewUtilStore(),
		pluginStore:     NewExecStore(),
		skillStore:      NewSkillStore(),
		transcriptStore: NewFlatDirStore(transcriptDir, 0444),
		tmpStore:        NewFlatDirStore(tmpDir, 0600),
	}
	s.sessionStore = store.NewSessionStore(store.SessionStoreConfig{
		AgentsDir:   agentsDir,
		SessionsDir: sessionsDir,
		Log:         s.log,
		Sink:        s.sink,
		SaveTranscript: func(data []byte) error {
			name := time.Now().Format("20060102T150405") + "-chat.md"
			return storeWrite(s.transcriptStore, name, data)
		},
		OnRename: func(oldID, newID string) {
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
		},
	})
	return s
}


// defaultTranscriptDir returns the transcript directory from OLLIE_TRANSCRIPT_PATH or the default.
func defaultTranscriptDir() string {
	if p := os.Getenv("OLLIE_TRANSCRIPT_PATH"); p != "" {
		return p
	}
	return paths.CfgDir() + "/transcript"
}

// defaultMemDir returns the memory directory from OLLIE_MEMORY_PATH or the default.
func defaultMemDir() string {
	if p := os.Getenv("OLLIE_MEMORY_PATH"); p != "" {
		return p
	}
	return paths.CfgDir() + "/memory"
}

// defaultTmpDir returns the tmp directory from OLLIE_TMP_PATH or the default.
func defaultTmpDir() string {
	if p := os.Getenv("OLLIE_TMP_PATH"); p != "" {
		return p
	}
	return paths.DataDir() + "/tmp"
}

// sessionFileStore returns a Store for the given session ID.
func (s *Server) sessionFileStore(sessID string) (store.RunnableStore, bool) {
	st, err := s.sessionStore.OpenStore(sessID)
	if err != nil {
		return nil, false
	}
	return st, true
}

// storeRead opens an entry in a store and reads it.
func storeRead(s store.Store, name string) ([]byte, error) {
	e, err := s.Open(name)
	if err != nil {
		return nil, err
	}
	return e.Read()
}

// storeWrite opens an entry in a store and writes to it.
func storeWrite(s store.Store, name string, data []byte) error {
	e, err := s.Open(name)
	if err != nil {
		return err
	}
	return e.Write(data)
}

// readTimeout is the server-side deadline for all non-blocking store reads.
const readTimeout = 10 * time.Second

// storeReadCtx is like storeRead but aborts if the context is cancelled or
// the read takes longer than readTimeout.
func storeReadCtx(ctx context.Context, s store.Store, name string) ([]byte, error) {
	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		data, err := storeRead(s, name)
		ch <- result{data, err}
	}()
	timer := time.NewTimer(readTimeout)
	defer timer.Stop()
	select {
	case r := <-ch:
		return r.data, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, fmt.Errorf("read timeout")
	}
}

// storeBlockingRead opens an entry and performs a blocking read.
func storeBlockingRead(s store.Store, name string, ctx context.Context, base string) ([]byte, error) {
	e, err := s.Open(name)
	if err != nil {
		return nil, err
	}
	return e.BlockingRead(ctx, base)
}

// Serve handles a single 9P connection. Each request is dispatched to its own
// goroutine so blocking reads (e.g. *wait files) do not stall the serve loop.
func (s *Server) Serve(conn net.Conn) {
	defer conn.Close()
	connCtx, connCancel := context.WithCancel(context.Background())
	cs := &connState{
		fids:    make(map[uint32]*fid),
		ctx:     connCtx,
		cancel:  connCancel,
		pending: make(map[uint16]context.CancelFunc),
	}
	s.mu.Lock()
	s.conns = append(s.conns, cs)
	s.mu.Unlock()
	defer func() {
		connCancel()
		s.mu.Lock()
		for i, c := range s.conns {
			if c == cs {
				s.conns = append(s.conns[:i], s.conns[i+1:]...)
				break
			}
		}
		s.mu.Unlock()
	}()

	responses := make(chan *plan9.Fcall, 16)
	var wg sync.WaitGroup

	// writer: serialises responses back onto the connection.
	go func() {
		for resp := range responses {
			plan9.WriteFcall(conn, resp) //nolint:errcheck
		}
	}()

	for {
		fc, err := plan9.ReadFcall(conn)
		if err != nil {
			if err != io.EOF {
				s.log.Error("read: %v", err)
			}
			break
		}
		reqCtx, reqCancel := context.WithCancel(connCtx)
		cs.mu.Lock()
		cs.pending[fc.Tag] = reqCancel
		cs.mu.Unlock()

		wg.Add(1)
		go func(fc *plan9.Fcall, ctx context.Context) {
			defer func() {
				reqCancel()
				cs.mu.Lock()
				delete(cs.pending, fc.Tag)
				cs.mu.Unlock()
				wg.Done()
			}()
			responses <- s.handle(cs, fc, ctx)
		}(fc, reqCtx)
	}

	wg.Wait()
	close(responses)
}

func (s *Server) handle(cs *connState, fc *plan9.Fcall, ctx context.Context) *plan9.Fcall {
	switch fc.Type {
	case plan9.Tversion:
		msize := fc.Msize
		if msize > 65536 {
			msize = 65536
		}
		s.log.Debug("Tversion msize=%d", msize)
		return &plan9.Fcall{Type: plan9.Rversion, Tag: fc.Tag, Msize: msize, Version: "9P2000"}
	case plan9.Tauth:
		return errFcall(fc, "no auth required")
	case plan9.Tattach:
		s.log.Debug("Tattach fid=%d", fc.Fid)
		return s.attach(cs, fc)
	case plan9.Twalk:
		return s.walk(cs, fc)
	case plan9.Topen:
		return s.open(cs, fc)
	case plan9.Tcreate:
		return s.create(cs, fc)
	case plan9.Tread:
		return s.read(cs, fc, ctx)
	case plan9.Twrite:
		return s.write(cs, fc)
	case plan9.Tstat:
		return s.stat(cs, fc)
	case plan9.Twstat:
		return s.wstat(cs, fc)
	case plan9.Tflush:
		cs.mu.Lock()
		if cancel, ok := cs.pending[fc.Oldtag]; ok {
			cancel()
		}
		cs.mu.Unlock()
		return &plan9.Fcall{Type: plan9.Rflush, Tag: fc.Tag}
	case plan9.Tclunk:
		return s.clunk(cs, fc)
	case plan9.Tremove:
		return s.remove(cs, fc)
	default:
		return errFcall(fc, "unsupported operation")
	}
}

// isSessionStoreFile reports whether path is a fixed file directly under /s/
// (i.e. /s/<name> where name is in sessionStoreFiles).
func isSessionStoreFile(path string) bool {
	name, ok := strings.CutPrefix(path, "/s/")
	if !ok || strings.Contains(name, "/") {
		return false
	}
	_, ok = store.SessionStoreFileMode(name)
	return ok
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
	case len(parts) == 1 && parts[0] == "sk":
		return "dir"
	case len(parts) == 1 && parts[0] == "t":
		return "dir"
	case len(parts) == 1 && parts[0] == "u":
		return "dir"
	case len(parts) == 2 && parts[0] == "u":
		if _, err := s.utilStore.Stat(parts[1]); err == nil {
			return "file"
		}
	case len(parts) == 1 && parts[0] == "x":
		return "dir"
	case len(parts) == 2 && parts[0] == "x":
		if _, err := s.pluginStore.Stat(parts[1]); err == nil {
			return "file"
		}
	case len(parts) == 1 && parts[0] == "tr":
		return "dir"
	case len(parts) == 2 && parts[0] == "tr":
		if _, err := s.transcriptStore.Stat(parts[1]); err == nil {
			return "file"
		}

	case len(parts) == 1 && parts[0] == "tmp":
		return "dir"
	case len(parts) == 2 && parts[0] == "tmp":
		if _, err := s.tmpStore.Stat(parts[1]); err == nil {
			return "file"
		}
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
	case len(parts) == 2 && parts[0] == "sk":
		if _, err := s.skillStore.Stat(parts[1]); err == nil {
			return "file"
		}
	case len(parts) == 2 && parts[0] == "t":
		if _, err := s.toolStore.Stat(parts[1]); err == nil {
			return "file"
		}
	case len(parts) == 3 && parts[0] == "s":
		if sfs, ok := s.sessionFileStore(parts[1]); ok {
			if _, err := sfs.Stat(parts[2]); err == nil {
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

	s.log.Debug("Twalk fid=%d newfid=%d from=%q wnames=%v", fc.Fid, fc.Newfid, f.path, fc.Wname)

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
	s.log.Debug("Topen fid=%d path=%q mode=%d", fc.Fid, f.path, fc.Mode)
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
	s.log.Debug("Tcreate parent=%q name=%q", f.path, fc.Name)
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
	case "/t":
		if err := s.toolStore.Create(fc.Name); err != nil {
			return errFcall(fc, err.Error())
		}
	case "/tmp":
		if err := s.tmpStore.Create(fc.Name); err != nil {
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

func (s *Server) read(cs *connState, fc *plan9.Fcall, ctx context.Context) *plan9.Fcall {
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
		s.log.Debug("Tread dir path=%q offset=%d count=%d", path, fc.Offset, fc.Count)
		data := s.readDir(path, fc.Offset, fc.Count)
		s.log.Debug("Rread dir path=%q len=%d", path, len(data))
		return &plan9.Fcall{Type: plan9.Rread, Tag: fc.Tag, Count: uint32(len(data)), Data: data}
	}

	// Fixed files directly under /s/ are served from the session store.
	if isSessionStoreFile(path) {
		s.log.Debug("Tread path=%q offset=%d count=%d", path, fc.Offset, fc.Count)
		content, err := storeReadCtx(ctx, s.sessionStore, pathBase(path))
		if err != nil {
			s.log.Debug("Rread path=%q err=%v", path, err)
			return errFcall(fc, err.Error())
		}
		s.log.Debug("Rread path=%q content_len=%d", path, len(content))
		return s.readSlice(fc, content)
	}

	// backends is a static list of ollie-provided backends.
	if path == "/backends" {
		s.log.Debug("Tread path=%q offset=%d count=%d", path, fc.Offset, fc.Count)
		content := []byte(strings.Join(backend.Backends(), "\n") + "\n")
		return s.readSlice(fc, content)
	}

	// help is served from ~/.config/ollie/help.md.
	if path == "/help" {
		s.log.Debug("Tread path=%q offset=%d count=%d", path, fc.Offset, fc.Count)
		content, err := os.ReadFile(s.helpPath())
		if err != nil {
			return errFcall(fc, err.Error())
		}
		return s.readSlice(fc, content)
	}

	// Agent config files are served from the agent store.
	if strings.HasPrefix(path, "/a/") {
		s.log.Debug("Tread path=%q offset=%d count=%d", path, fc.Offset, fc.Count)
		content, err := storeReadCtx(ctx, s.agentStore, pathBase(path))
		if err != nil {
			return errFcall(fc, err.Error())
		}
		return s.readSlice(fc, content)
	}

	// Prompt files are served from the prompt store.
	if strings.HasPrefix(path, "/p/") {
		s.log.Debug("Tread path=%q offset=%d count=%d", path, fc.Offset, fc.Count)
		content, err := storeReadCtx(ctx, s.promptStore, pathBase(path))
		if err != nil {
			return errFcall(fc, err.Error())
		}
		return s.readSlice(fc, content)
	}

	// Memory files are served from the memory store.
	if strings.HasPrefix(path, "/m/") {
		s.log.Debug("Tread path=%q offset=%d count=%d", path, fc.Offset, fc.Count)
		content, err := storeReadCtx(ctx, s.memStore, pathBase(path))
		if err != nil {
			return errFcall(fc, err.Error())
		}
		return s.readSlice(fc, content)
	}


	// Skill files are served from the skill store.
	if strings.HasPrefix(path, "/sk/") {
		s.log.Debug("Tread path=%q offset=%d count=%d", path, fc.Offset, fc.Count)
		content, err := storeReadCtx(ctx, s.skillStore, pathBase(path))
		if err != nil {
			return errFcall(fc, err.Error())
		}
		return s.readSlice(fc, content)
	}

	// Tool files are served from the tool store.
	if strings.HasPrefix(path, "/t/") {
		s.log.Debug("Tread path=%q offset=%d count=%d", path, fc.Offset, fc.Count)
		content, err := storeReadCtx(ctx, s.toolStore, pathBase(path))
		if err != nil {
			return errFcall(fc, err.Error())
		}
		return s.readSlice(fc, content)
	}

	// Util files are served from the util store.
	if strings.HasPrefix(path, "/u/") {
		s.log.Debug("Tread path=%q offset=%d count=%d", path, fc.Offset, fc.Count)
		content, err := storeReadCtx(ctx, s.utilStore, pathBase(path))
		if err != nil {
			return errFcall(fc, err.Error())
		}
		return s.readSlice(fc, content)
	}

	// Plugin files are served from the plugin store.
	if strings.HasPrefix(path, "/x/") {
		s.log.Debug("Tread path=%q offset=%d count=%d", path, fc.Offset, fc.Count)
		content, err := storeReadCtx(ctx, s.pluginStore, pathBase(path))
		if err != nil {
			return errFcall(fc, err.Error())
		}
		return s.readSlice(fc, content)
	}

	// Tmp files are served from the tmp store.
	if strings.HasPrefix(path, "/tmp/") {
		s.log.Debug("Tread path=%q offset=%d count=%d", path, fc.Offset, fc.Count)
		content, err := storeReadCtx(ctx, s.tmpStore, pathBase(path))
		if err != nil {
			return errFcall(fc, err.Error())
		}
		return s.readSlice(fc, content)
	}

	// Transcript files are served from the transcript store.
	if strings.HasPrefix(path, "/tr/") {
		s.log.Debug("Tread path=%q offset=%d count=%d", path, fc.Offset, fc.Count)
		content, err := storeReadCtx(ctx, s.transcriptStore, pathBase(path))
		if err != nil {
			return errFcall(fc, err.Error())
		}
		return s.readSlice(fc, content)
	}

	// Session files: /s/{id}/{file}
	if strings.HasPrefix(path, "/s/") {
		parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 3)
		if len(parts) == 3 {
			s.log.Debug("Tread session file path=%q offset=%d count=%d", path, fc.Offset, fc.Count)
			sfs, ok := s.sessionFileStore(parts[1])
			if !ok {
				s.log.Debug("Rread session not found: %s", parts[1])
				return &plan9.Fcall{Type: plan9.Rread, Tag: fc.Tag, Count: 0}
			}
			// fifo.out: non-zero offset is the trailing EOF read after a successful pop.
			if parts[2] == "fifo.out" && fc.Offset > 0 {
				return &plan9.Fcall{Type: plan9.Rread, Tag: fc.Tag, Count: 0}
			}
			// *wait files block until a value changes; use connection context.
			// One blocking read per open: a non-zero offset means the client
			// already received data this open and is now polling for EOF.
			if strings.HasSuffix(parts[2], "wait") {
				if fc.Offset > 0 {
					return &plan9.Fcall{Type: plan9.Rread, Tag: fc.Tag, Count: 0}
				}
				cs.mu.RLock()
				f, fidOK := cs.fids[fc.Fid]
				var base string
				if fidOK {
					base = f.waitBase
				}
				cs.mu.RUnlock()
				// Wrap with a short timeout so the FUSE read returns
				// periodically, allowing pending signals (e.g. SIGINT) to
				// be delivered to the blocked client process.
				waitCtx, waitCancel := context.WithTimeout(ctx, 5*time.Second)
				defer waitCancel()
				content, err := storeBlockingRead(sfs, parts[2], waitCtx, base)
				if err != nil {
					return errFcall(fc, err.Error())
				}
				// Update the fid's baseline to the returned value for subsequent reads.
				if content != nil {
					cs.mu.Lock()
					if f, ok := cs.fids[fc.Fid]; ok {
						f.waitBase = strings.TrimSuffix(string(content), "\n")
					}
					cs.mu.Unlock()
				}
				return s.readSlice(fc, content)
			}
			content, err := storeReadCtx(ctx, sfs, parts[2])
			if err != nil {
				s.log.Debug("Rread session file err=%v", err)
				return errFcall(fc, err.Error())
			}
			s.log.Debug("Rread session file path=%q content_len=%d", path, len(content))
			return s.readSlice(fc, content)
		}
	}
	s.log.Debug("Tread unhandled path=%q", path)
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
	return paths.CfgDir() + "/help.md"
}

func (s *Server) write(cs *connState, fc *plan9.Fcall) *plan9.Fcall {
	cs.mu.Lock()
	f, ok := cs.fids[fc.Fid]
	if !ok {
		cs.mu.Unlock()
		return errFcall(fc, "bad fid")
	}
	s.log.Debug("Twrite fid=%d path=%q offset=%d len=%d", fc.Fid, f.path, fc.Offset, len(fc.Data))
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
	s.log.Debug("Tstat path=%q mode=%o len=%d", f.path, dir.Mode, dir.Length)
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
	s.log.Debug("Twstat path=%q oldName=%q newName=%q", f.path, oldName, newDir.Name)
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

func (s *Server) clunk(cs *connState, fc *plan9.Fcall) *plan9.Fcall {
	cs.mu.Lock()
	f, ok := cs.fids[fc.Fid]
	var path string
	var data []byte
	var writable bool
	if ok {
		if m := f.mode & 3; m == plan9.OWRITE || m == plan9.ORDWR {
			path = f.path
			writable = true
			data = make([]byte, len(f.writeBuf))
			copy(data, f.writeBuf)
		}
		delete(cs.fids, fc.Fid)
	}
	cs.mu.Unlock()
	if writable {
		s.log.Debug("Tclunk flush path=%q writeBuf=%d", path, len(data))
		input := strings.TrimSpace(string(data))
		if s.isAsyncWrite(path) {
			go s.handleWrite(path, input) //nolint:errcheck
		} else if err := s.handleWrite(path, input); err != nil {
			s.log.Debug("Tclunk handleWrite err=%v", err)
			return errFcall(fc, err.Error())
		}
	} else {
		s.log.Debug("Tclunk fid=%d path=%q (no write)", fc.Fid, path)
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
	case "prompt", "fifo.in", "ctl":
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
	s.log.Debug("Tremove path=%q", path)
	var err error
	switch {
	case strings.HasPrefix(path, "/a/"):
		err = s.agentStore.Delete(pathBase(path))
	case strings.HasPrefix(path, "/m/"):
		err = s.memStore.Delete(pathBase(path))
	case strings.HasPrefix(path, "/sk/"):
		err = s.skillStore.Delete(pathBase(path))
	case strings.HasPrefix(path, "/t/"):
		err = s.toolStore.Delete(pathBase(path))
	case strings.HasPrefix(path, "/tmp/"):
		err = s.tmpStore.Delete(pathBase(path))
	case strings.HasPrefix(path, "/tr/"):
		err = s.transcriptStore.Delete(pathBase(path))
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
	s.log.Debug("handleWrite path=%q input_len=%d", path, len(input))

	if path == "/s/new" {
		if input == "" {
			return nil
		}
		return storeWrite(s.sessionStore, "new", []byte(input))
	}

	// Agent config writes go to the agent store.
	if strings.HasPrefix(path, "/a/") {
		return storeWrite(s.agentStore, pathBase(path), []byte(input))
	}

	// Memory file writes go to the memory store.
	if strings.HasPrefix(path, "/m/") {
		return storeWrite(s.memStore, pathBase(path), []byte(input))
	}

	// Skill file writes go to the skill store.
	if strings.HasPrefix(path, "/sk/") {
		return storeWrite(s.skillStore, pathBase(path), []byte(input))
	}

	// Tool file writes go to the tool store.
	if strings.HasPrefix(path, "/t/") {
		return storeWrite(s.toolStore, pathBase(path), []byte(input))
	}

	// Tmp file writes go to the tmp store.
	if strings.HasPrefix(path, "/tmp/") {
		return storeWrite(s.tmpStore, pathBase(path), []byte(input))
	}

	// Session file writes: /s/{sessid}/{file}
	if strings.HasPrefix(path, "/s/") {
		parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 3)
		if len(parts) == 3 {
			sfs, ok := s.sessionFileStore(parts[1])
			if !ok {
				return fmt.Errorf("session not found: %s", parts[1])
			}
			return storeWrite(sfs, parts[2], []byte(input))
		}
	}

	return nil
}

// Shutdown kills all active sessions and batch jobs.
func (s *Server) Shutdown() {
	s.sessionStore.Shutdown()
}

// InterruptAll cancels any in-progress agent turn on every active session.
func (s *Server) InterruptAll() {
	s.sessionStore.InterruptAll()
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
		dirs = append(dirs, makeDir("s", "/s", true, plan9.DMDIR|0555))
		dirs = append(dirs, makeDir("sk", "/sk", true, plan9.DMDIR|0555))
		dirs = append(dirs, makeDir("t", "/t", true, plan9.DMDIR|0777))
		dirs = append(dirs, makeDir("tmp", "/tmp", true, plan9.DMDIR|0755))
		dirs = append(dirs, makeDir("u", "/u", true, plan9.DMDIR|0755))
		dirs = append(dirs, makeDir("x", "/x", true, plan9.DMDIR|0555))
		dirs = append(dirs, makeDir("tr", "/tr", true, plan9.DMDIR|0555))
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
	} else if path == "/tmp" {
		entries, _ := s.tmpStore.List()
		for _, e := range entries {
			if !e.IsDir() {
				d := makeDir(e.Name(), "/tmp/"+e.Name(), false, 0600)
				if info, err := e.Info(); err == nil {
					d.Atime = uint32(info.ModTime().Unix())
					d.Mtime = uint32(info.ModTime().Unix())
				}
				dirs = append(dirs, d)
			}
		}
	} else if path == "/tr" {
		entries, _ := s.transcriptStore.List()
		for _, e := range entries {
			if !e.IsDir() {
				d := makeDir(e.Name(), "/tr/"+e.Name(), false, 0444)
				if info, err := e.Info(); err == nil {
					d.Atime = uint32(info.ModTime().Unix())
					d.Mtime = uint32(info.ModTime().Unix())
				}
				dirs = append(dirs, d)
			}
		}
	} else if path == "/u" {
		entries, _ := s.utilStore.List()
		for _, e := range entries {
			dirs = append(dirs, makeDir(e.Name(), "/u/"+e.Name(), false, 0555))
		}
	} else if path == "/x" {
		entries, _ := s.pluginStore.List()
		for _, e := range entries {
			dirs = append(dirs, makeDir(e.Name(), "/x/"+e.Name(), false, 0555))
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
		if sfs, ok := s.sessionFileStore(sessID); ok {
			entries, _ := sfs.List()
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
		} else if path == "/a" || path == "/m" || path == "/tmp" || path == "/u" {
			mode = plan9.DMDIR | 0755
		} else {
			mode = plan9.DMDIR | 0555
		}
	} else {
		switch base {
		case "ctl", "prompt", "fifo.in":
			mode = 0200
		case "chat", "usage", "cost", "ctxsz", "models", "fifo.out", "offset", "statewait", "systemprompt":
			mode = 0444
		case "cfg":
			mode = 0666
		case "tail":
			mode = 0555
		default:
			if path == "/backends" || path == "/help" {
				mode = 0444
			} else if isSessionStoreFile(path) {
				mode_, _ := store.SessionStoreFileMode(base)
				mode = plan9.Perm(mode_)
			} else if strings.HasPrefix(path, "/a/") {
				mode = 0666
			} else if strings.HasPrefix(path, "/p/") {
				mode = 0444
			} else if strings.HasPrefix(path, "/m/") {
				mode = 0666
			} else if path == "/sk/idx" {
				mode = 0444
			} else if strings.HasPrefix(path, "/sk/") {
				mode = 0666
			} else if path == "/t/idx" {
				mode = 0444
			} else if strings.HasPrefix(path, "/t/") {
				mode = 0777
			} else if strings.HasPrefix(path, "/u/") {
				mode = 0555
			} else if strings.HasPrefix(path, "/x/") {
				mode = 0555
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

	// For chat and tailable mutable files, report actual size and
	// Qid version so polling tools (tail -f) can detect changes via stat.
	// Path format: /s/{sessid}/{file}
	if strings.HasPrefix(path, "/s/") {
		parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 3)
		if len(parts) == 3 && parts[0] == "s" {
			if sess := s.sessionStore.Session(parts[1]); sess != nil {
				if base == "chat" {
					length, vers := sess.LogInfo()
					dir.Length = uint64(length)
					dir.Qid.Vers = vers
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

	// For all other readable files, compute content length so clients
	// that check stat before reading (cat, 9pfuse, etc.) see non-zero size.
	if dir.Length == 0 && !isDir {
		switch {
		case isSessionStoreFile(path):
			if content, err := storeRead(s.sessionStore, base); err == nil {
				dir.Length = uint64(len(content))
			}
		case path == "/backends":
			dir.Length = uint64(len(strings.Join(backend.Backends(), "\n") + "\n"))
		case path == "/help":
			if info, err := os.Stat(s.helpPath()); err == nil {
				dir.Length = uint64(info.Size())
			}
		case strings.HasPrefix(path, "/a/"):
			if info, err := s.agentStore.Stat(base); err == nil {
				dir.Length = uint64(info.Size())
			}
		case strings.HasPrefix(path, "/p/"):
			if info, err := s.promptStore.Stat(base); err == nil {
				dir.Length = uint64(info.Size())
			}
		case strings.HasPrefix(path, "/sk/"):
			if content, err := storeRead(s.skillStore, base); err == nil {
				dir.Length = uint64(len(content))
			}
		case strings.HasPrefix(path, "/t/"):
			if content, err := storeRead(s.toolStore, base); err == nil {
				dir.Length = uint64(len(content))
			}
		case strings.HasPrefix(path, "/u/"):
			if content, err := storeRead(s.utilStore, base); err == nil {
				dir.Length = uint64(len(content))
			}
		case strings.HasPrefix(path, "/x/"):
			if content, err := storeRead(s.pluginStore, base); err == nil {
				dir.Length = uint64(len(content))
			}
		case strings.HasPrefix(path, "/tmp/"):
			if info, err := s.tmpStore.Stat(base); err == nil {
				dir.Length = uint64(info.Size())
				dir.Atime = uint32(info.ModTime().Unix())
				dir.Mtime = uint32(info.ModTime().Unix())
			}
		case strings.HasPrefix(path, "/tr/"):
			if info, err := s.transcriptStore.Stat(base); err == nil {
				dir.Length = uint64(info.Size())
				dir.Atime = uint32(info.ModTime().Unix())
				dir.Mtime = uint32(info.ModTime().Unix())
			}
		case strings.HasPrefix(path, "/s/"):
			parts := strings.SplitN(strings.TrimPrefix(path, "/s/"), "/", 2)
			if len(parts) == 2 {
				if sfs, ok := s.sessionFileStore(parts[0]); ok {
					if info, err := sfs.Stat(parts[1]); err == nil {
						dir.Length = uint64(info.Size())
					}
				}
			}
		}
	}

	return dir
}

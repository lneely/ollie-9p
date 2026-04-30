package store_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ollie/pkg/agent"
	"ollie/pkg/backend"
	olog "ollie/pkg/log"
	"olliesrv/store"
)

// --- stub agent.Core ---

type stubCore struct {
	state     string
	running   bool
	backend   string
	model     string
	agentName string
	cwd       string
	usage     string
	ctxsz     string
	models    string
	sysprompt string
	reply     string
	params    backend.GenerationParams
	closed    bool

	// For testing WaitChange: send a value to unblock.
	waitCh chan string

	// Records
	submitted  []string
	queued     []string
	interrupted     bool
	setSessionIDErr error
	submitCh        chan struct{} // closed after each Submit call
}

func (c *stubCore) Submit(_ context.Context, input string, handler agent.EventHandler) {
	c.submitted = append(c.submitted, input)
	if handler != nil && c.reply != "" {
		handler(agent.Event{Role: "assistant", Content: c.reply})
	}
	if c.submitCh != nil {
		close(c.submitCh)
	}
}
func (c *stubCore) Interrupt(error) bool                                        { c.interrupted = true; return c.running }
func (c *stubCore) Inject(string)                                               {}
func (c *stubCore) Queue(s string)                                              { c.queued = append(c.queued, s) }
func (c *stubCore) PopQueue() (string, bool) {
	if len(c.queued) == 0 {
		return "", false
	}
	s := c.queued[0]
	c.queued = c.queued[1:]
	return s, true
}
func (c *stubCore) IsRunning() bool                                             { return c.running }
func (c *stubCore) State() string                                               { return c.state }
func (c *stubCore) Reply() string                                               { return c.reply }
func (c *stubCore) AgentName() string                                           { return c.agentName }
func (c *stubCore) BackendName() string                                         { return c.backend }
func (c *stubCore) ModelName() string                                           { return c.model }
func (c *stubCore) CtxSz() string                                               { return c.ctxsz }
func (c *stubCore) Usage() string                                               { return c.usage }
func (c *stubCore) Cost() string                                                { return "" }
func (c *stubCore) ListModels() string                                          { return c.models }
func (c *stubCore) CWD() string                                                 { return c.cwd }
func (c *stubCore) SetCWD(dir string) error                                     { c.cwd = dir; return nil }
func (c *stubCore) SetSessionID(string) error                                   { return c.setSessionIDErr }
func (c *stubCore) Context() []backend.Message                                 { return nil }
func (c *stubCore) SystemPrompt() string                                        { return c.sysprompt }
func (c *stubCore) GenerationParams() backend.GenerationParams                  { return c.params }
func (c *stubCore) SetGenerationParams(p backend.GenerationParams) error        { c.params = p; return nil }
func (c *stubCore) SetEnv(string, string)                                       {}
func (c *stubCore) WaitChange(ctx context.Context, _, _ string) (string, bool) {
	if c.waitCh != nil {
		select {
		case v := <-c.waitCh:
			return v, true
		case <-ctx.Done():
			return "", false
		}
	}
	<-ctx.Done()
	return "", false
}
func (c *stubCore) Close()                                                      { c.closed = true }

// publishCore wraps stubCore and emits a realistic event sequence on Submit.
type publishCore struct {
	*stubCore
}

func (c *publishCore) Submit(_ context.Context, input string, handler agent.EventHandler) {
	c.submitted = append(c.submitted, input)
	if handler == nil {
		return
	}
	handler(agent.Event{Role: "user", Content: input})
	handler(agent.Event{Role: "assistant", Content: "thinking..."})
	handler(agent.Event{Role: "call", Name: "fn", Content: "arg1"})
	handler(agent.Event{Role: "tool", Content: "result"})
	handler(agent.Event{Role: "assistant", Content: "done"})
}

// blockingCore wraps stubCore but blocks on Submit until ctx is cancelled.
type blockingCore struct {
	*stubCore
}

func (c *blockingCore) Submit(ctx context.Context, input string, handler agent.EventHandler) {
	c.submitted = append(c.submitted, input)
	<-ctx.Done()
}

// --- helpers ---

func testSink() *olog.Sink {
	return olog.NewSink(io.Discard, io.Discard, olog.LevelError)
}

func testSession(id string) *store.Session {
	ctx, cancel := context.WithCancel(context.Background())
	return store.NewSession(id, &stubCore{state: "idle", backend: "stub", model: "m", agentName: "default", cwd: "/tmp"}, ctx, cancel)
}

func seedSkill(t *testing.T, base, name string) {
	t.Helper()
	d := filepath.Join(base, name)
	os.MkdirAll(d, 0755)
	os.WriteFile(filepath.Join(d, "SKILL.md"), []byte("---\ndescription: test\n---\n"), 0644)
}

func newSessionStore(t *testing.T) *store.SessionStore {
	t.Helper()
	sink := testSink()
	return store.NewSessionStore(store.SessionStoreConfig{
		Log:      sink.NewLogger("test"),
		Sink:     sink,
		ReadFile: func(string) ([]byte, error) { return []byte("#!/bin/sh\n"), nil },
		MkdirAll: func(string, os.FileMode) error { return nil },
	})
}

func newSessionStoreWithCore(t *testing.T) *store.SessionStore {
	t.Helper()
	sink := testSink()
	return store.NewSessionStore(store.SessionStoreConfig{
		Log:      sink.NewLogger("test"),
		Sink:     sink,
		ReadFile: func(string) ([]byte, error) { return []byte("#!/bin/sh\n"), nil },
		MkdirAll: func(string, os.FileMode) error { return nil },
		NewCore: func(sessionID, agentName, cwd string) (agent.Core, error) {
			return &stubCore{state: "idle", backend: "stub", model: "m", agentName: agentName, cwd: cwd}, nil
		},
	})
}


func newSessionFileStore(t *testing.T, sess *store.Session) (*store.SessionFileStore, *stubCore) {
	t.Helper()
	sink := testSink()
	core := sess.Core.(*stubCore)
	var killed bool
	var renamed string
	var saved []byte
	sf := store.NewSessionFileStore(sess, sink.NewLogger("test"),
		func() { killed = true },
		func(id string) error { renamed = id; return nil },
		func(data []byte) error { saved = data; return nil },
	)
	_ = killed
	_ = renamed
	_ = saved
	return sf, core
}

// newSessionFileStoreWith returns a SessionFileStore with custom callbacks.
func newSessionFileStoreWith(t *testing.T, sess *store.Session, kill func(), rename func(string) error, save func([]byte) error) *store.SessionFileStore {
	t.Helper()
	sink := testSink()
	return store.NewSessionFileStore(sess, sink.NewLogger("test"), kill, rename, save)
}

// storeRead is a test helper: Open + Read.
func storeRead(t *testing.T, s store.Store, name string) []byte {
	t.Helper()
	e, err := s.Open(name)
	if err != nil {
		t.Fatalf("Open(%q): %v", name, err)
	}
	data, err := e.Read()
	if err != nil {
		t.Fatalf("Read(%q): %v", name, err)
	}
	return data
}

// storeWrite is a test helper: Open + Write.
func storeWrite(t *testing.T, s store.Store, name string, data []byte) {
	t.Helper()
	e, err := s.Open(name)
	if err != nil {
		t.Fatalf("Open(%q): %v", name, err)
	}
	if err := e.Write(data); err != nil {
		t.Fatalf("Write(%q): %v", name, err)
	}
}

// --- contract checks ---

func checkReadableContract(t *testing.T, s store.Store, name string) {
	t.Helper()
	fi, err := s.Stat(name)
	if err != nil {
		t.Fatalf("Stat(%q): %v", name, err)
	}
	if fi.Name() != name {
		t.Errorf("Stat(%q).Name() = %q", name, fi.Name())
	}
	if _, err := s.Stat("__nonexistent__"); err == nil {
		t.Error("Stat(nonexistent) should error")
	}
	entries, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Name() == name {
			found = true
		}
	}
	if !found {
		t.Errorf("List() missing %q", name)
	}
	e, err := s.Open(name)
	if err != nil {
		t.Fatalf("Open(%q): %v", name, err)
	}
	if _, err := e.Read(); err != nil {
		t.Fatalf("Read(%q): %v", name, err)
	}
}

func checkReadWriteContract(t *testing.T, s store.Store, name string) {
	t.Helper()
	want := []byte("hello")
	e, err := s.Open(name)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := e.Write(want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := e.Read()
	if err != nil {
		t.Fatalf("Read after Write: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("Read = %q; want %q", got, want)
	}
	want2 := []byte("world")
	if err := e.Write(want2); err != nil {
		t.Fatalf("Write overwrite: %v", err)
	}
	got2, err := e.Read()
	if err != nil {
		t.Fatalf("Read after overwrite: %v", err)
	}
	if string(got2) != string(want2) {
		t.Errorf("Read after overwrite = %q; want %q", got2, want2)
	}
	checkReadableContract(t, s, name)
}

func checkStoreContract(t *testing.T, s store.Store, name string) {
	t.Helper()
	if err := s.Create(name); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.Stat(name); err != nil {
		t.Fatalf("Stat after Create: %v", err)
	}
	renamed := name + "-renamed"
	if err := s.Rename(name, renamed); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if _, err := s.Stat(name); err == nil {
		t.Error("Stat(old) should error after Rename")
	}
	if _, err := s.Stat(renamed); err != nil {
		t.Errorf("Stat(new) after Rename: %v", err)
	}
	s.Delete(renamed) //nolint:errcheck
	if err := s.Create(name); err != nil {
		t.Fatalf("Create after rename cleanup: %v", err)
	}
	if err := s.Delete(name); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Stat(name); err == nil {
		t.Error("Stat after Delete should error")
	}
	if err := s.Delete("__nonexistent__"); err == nil {
		t.Error("Delete(nonexistent) should error")
	}
	checkReadWriteContract(t, s, name)
}

// ===== FlatDir =====

func TestFlatDirContract(t *testing.T) {
	checkStoreContract(t, store.NewFlatDir(t.TempDir(), 0644), "test-file")
}

func TestFlatDirCreateMkdirError(t *testing.T) {
	fd := store.NewFlatDir("/nonexistent/path", 0644)
	if err := fd.Create("f"); err == nil {
		t.Error("Create should fail when dir doesn't exist")
	}
}

func TestFlatDirPutMkdirError(t *testing.T) {
	fd := store.NewFlatDir("/nonexistent/path", 0644)
	e, err := fd.Open("f")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := e.Write([]byte("x")); err == nil {
		t.Error("Write should fail when dir doesn't exist")
	}
}

// ===== SkillStore =====

func TestSkillStoreContract(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OLLIE_SKILLS_PATH", dir)
	seedSkill(t, dir, "test-skill")

	s := store.NewSkillStore()

	// Rename (needs seeded skill with valid front matter)
	seedSkill(t, dir, "rename-src")
	if err := s.Rename("rename-src.md", "rename-dst.md"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if _, err := s.Stat("rename-src.md"); err == nil {
		t.Error("Stat(old) should error after Rename")
	}
	if _, err := s.Stat("rename-dst.md"); err != nil {
		t.Errorf("Stat(new) after Rename: %v", err)
	}
	s.Delete("rename-dst.md") //nolint:errcheck

	// Delete
	seedSkill(t, dir, "del-test")
	if err := s.Delete("del-test.md"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Stat("del-test.md"); err == nil {
		t.Error("Stat after Delete should error")
	}
	if err := s.Delete("__nonexistent__.md"); err == nil {
		t.Error("Delete(nonexistent) should error")
	}

	// Write/Read round-trip (needs valid front matter)
	content := []byte("---\ndescription: rw test\n---\nbody\n")
	if err := s.Create("rw-skill.md"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	storeWrite(t, s, "rw-skill.md", content)
	got := storeRead(t, s, "rw-skill.md")
	if string(got) != string(content) {
		t.Errorf("Read = %q; want %q", got, content)
	}

	// Readable
	checkReadableContract(t, s, "test-skill.md")

	// idx
	idx := storeRead(t, s, "idx")
	if len(idx) == 0 {
		t.Error("idx should be non-empty with seeded skills")
	}

	// Rename nonexistent
	if err := s.Rename("__nope__.md", "x.md"); err == nil {
		t.Error("Rename(nonexistent) should error")
	}

	// Create calls through injected mkdirAll + writeFile
	var createdDir, createdFile string
	s2 := store.NewSkillStoreWith(store.SkillStoreConfig{
		Dirs:      []string{dir},
		MkdirAll:  func(p string, _ os.FileMode) error { createdDir = p; return nil },
		WriteFile: func(p string, _ []byte, _ os.FileMode) error { createdFile = p; return nil },
	})
	if err := s2.Create("newskill.md"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !strings.HasSuffix(createdDir, "newskill") {
		t.Errorf("mkdirAll path = %q; want suffix newskill", createdDir)
	}
	if !strings.HasSuffix(createdFile, "SKILL.md") {
		t.Errorf("writeFile path = %q; want suffix SKILL.md", createdFile)
	}

	// Create mkdirAll error
	s3 := store.NewSkillStoreWith(store.SkillStoreConfig{
		Dirs:     []string{dir},
		MkdirAll: func(string, os.FileMode) error { return fmt.Errorf("denied") },
	})
	if err := s3.Create("x.md"); err == nil {
		t.Error("Create should fail when mkdirAll errors")
	}
}

// ===== Session =====

func TestSessionAppendLog(t *testing.T) {
	sess := testSession("s1")
	defer sess.Cancel()

	sess.AppendLog([]byte("hello"))
	sess.AppendLog(nil) // no-op
	sess.AppendLog([]byte(" world"))

	l, v := sess.LogInfo()
	if l != 11 {
		t.Errorf("ChatInfo length = %d; want 11", l)
	}
	if v != 2 {
		t.Errorf("ChatInfo vers = %d; want 2", v)
	}
}

func TestSessionStoreFileMode(t *testing.T) {
	if m, ok := store.SessionStoreFileMode("new"); !ok || m != 0666 {
		t.Errorf("SessionStoreFileMode(new) = %o, %v", m, ok)
	}
	if _, ok := store.SessionStoreFileMode("bogus"); ok {
		t.Error("SessionStoreFileMode(bogus) should be false")
	}
}

// ===== SessionStore =====

func TestSessionStoreReadableContract(t *testing.T) {
	checkReadableContract(t, newSessionStore(t), "new")
}

func TestSessionStoreGetIdx(t *testing.T) {
	s := newSessionStore(t)
	sess := testSession("abc")
	defer sess.Cancel()
	s.AddSession(sess)

	data := storeRead(t, s, "idx")
	if !strings.Contains(string(data), "abc") {
		t.Errorf("idx = %q; want to contain abc", data)
	}
}

func TestSessionStoreGetScript(t *testing.T) {
	s := newSessionStore(t)
	data := storeRead(t, s, "ls")
	if string(data) != "#!/bin/sh\n" {
		t.Errorf("Read(ls) = %q", data)
	}
}

func TestSessionStoreStatSession(t *testing.T) {
	s := newSessionStore(t)
	sess := testSession("s1")
	defer sess.Cancel()
	s.AddSession(sess)

	fi, err := s.Stat("s1")
	if err != nil {
		t.Fatalf("Stat(s1): %v", err)
	}
	if !fi.IsDir() {
		t.Error("session stat should be dir")
	}
}

func TestSessionStoreListIncludesSessions(t *testing.T) {
	s := newSessionStore(t)
	sess := testSession("s1")
	defer sess.Cancel()
	s.AddSession(sess)

	entries, _ := s.List()
	found := false
	for _, e := range entries {
		if e.Name() == "s1" {
			found = true
		}
	}
	if !found {
		t.Error("List() missing session s1")
	}
}

func TestSessionStoreWriteNotWritable(t *testing.T) {
	s := newSessionStore(t)
	e, err := s.Open("idx")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := e.Write(nil); err == nil {
		t.Error("Write(idx) should error")
	}
}

func TestSessionStoreCreateErrors(t *testing.T) {
	s := newSessionStore(t)
	if err := s.Create("x"); err == nil {
		t.Error("Create should always error")
	}
}

func TestSessionStoreDeleteAndKill(t *testing.T) {
	s := newSessionStore(t)
	sess := testSession("s1")
	s.AddSession(sess)

	if err := s.Delete("s1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if s.Session("s1") != nil {
		t.Error("session should be gone after Delete")
	}
	core := sess.Core.(*stubCore)
	if !core.closed {
		t.Error("core should be closed after Delete")
	}
	if err := s.Delete("nope"); err == nil {
		t.Error("Delete(nonexistent) should error")
	}
}

func TestSessionStoreSession(t *testing.T) {
	s := newSessionStore(t)
	if s.Session("nope") != nil {
		t.Error("Session(nonexistent) should be nil")
	}
	sess := testSession("s1")
	defer sess.Cancel()
	s.AddSession(sess)
	if s.Session("s1") == nil {
		t.Error("Session(s1) should not be nil")
	}
}

func TestSessionStoreInterruptAll(t *testing.T) {
	s := newSessionStore(t)
	sess := testSession("s1")
	defer sess.Cancel()
	sess.Core.(*stubCore).running = true
	s.AddSession(sess)

	s.InterruptAll() // should not panic
}

func TestSessionStoreShutdown(t *testing.T) {
	s := newSessionStore(t)
	sess := testSession("s1")
	s.AddSession(sess)

	s.Shutdown()
	if s.Session("s1") != nil {
		t.Error("session should be gone after Shutdown")
	}
}

func TestSessionStoreRename(t *testing.T) {
	sink := testSink()
	var renamed [2]string
	s := store.NewSessionStore(store.SessionStoreConfig{
		Log:      sink.NewLogger("test"),
		Sink:     sink,
		ReadFile: func(string) ([]byte, error) { return nil, nil },
		MkdirAll: func(string, os.FileMode) error { return nil },
		OnRename: func(oldID, newID string) { renamed = [2]string{oldID, newID} },
	})
	sess := testSession("old")
	defer sess.Cancel()
	s.AddSession(sess)

	if err := s.Rename("old", "new"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if s.Session("old") != nil {
		t.Error("old session should be gone")
	}
	if s.Session("new") == nil {
		t.Error("new session should exist")
	}
	if renamed != [2]string{"old", "new"} {
		t.Errorf("OnRename called with %v; want [old new]", renamed)
	}
}

func TestSessionStoreRenameErrors(t *testing.T) {
	s := newSessionStore(t)
	// nonexistent
	if err := s.Rename("nope", "x"); err == nil {
		t.Error("Rename(nonexistent) should error")
	}
	// duplicate
	s.AddSession(testSession("a"))
	s.AddSession(testSession("b"))
	if err := s.Rename("a", "b"); err == nil {
		t.Error("Rename to existing should error")
	}
	// running
	sess := testSession("r")
	sess.Core.(*stubCore).running = true
	s.AddSession(sess)
	if err := s.Rename("r", "r2"); err == nil {
		t.Error("Rename while running should error")
	}
	// SetSessionID error
	sess2 := testSession("sid")
	sess2.Core.(*stubCore).setSessionIDErr = fmt.Errorf("id error")
	s.AddSession(sess2)
	if err := s.Rename("sid", "sid2"); err == nil {
		t.Error("Rename with SetSessionID error should error")
	}
}

// ===== SessionFileStore =====

func TestSessionFileStoreReadableContract(t *testing.T) {
	sess := testSession("s1")
	defer sess.Cancel()
	sink := testSink()
	sf := store.NewSessionFileStore(sess, sink.NewLogger("test"),
		func() {}, func(string) error { return nil }, func([]byte) error { return nil })
	checkReadableContract(t, sf, "cfg")
}

func TestSessionFileStoreList(t *testing.T) {
	sess := testSession("s1")
	defer sess.Cancel()
	sink := testSink()
	sf := store.NewSessionFileStore(sess, sink.NewLogger("test"),
		func() {}, func(string) error { return nil }, func([]byte) error { return nil })

	entries, err := sf.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != len(store.SessionFileList) {
		t.Errorf("List() returned %d entries; want %d", len(entries), len(store.SessionFileList))
	}
}

func TestSessionFileStoreStatChat(t *testing.T) {
	sess := testSession("s1")
	defer sess.Cancel()
	sess.AppendLog([]byte("hello"))
	sink := testSink()
	sf := store.NewSessionFileStore(sess, sink.NewLogger("test"),
		func() {}, func(string) error { return nil }, func([]byte) error { return nil })

	fi, err := sf.Stat("chat")
	if err != nil {
		t.Fatalf("Stat(chat): %v", err)
	}
	if fi.Size() != 5 {
		t.Errorf("Stat(chat).Size() = %d; want 5", fi.Size())
	}
}

func TestSessionFileStoreGetChat(t *testing.T) {
	sess := testSession("s1")
	defer sess.Cancel()
	sess.AppendLog([]byte("hello"))
	sink := testSink()
	sf := store.NewSessionFileStore(sess, sink.NewLogger("test"),
		func() {}, func(string) error { return nil }, func([]byte) error { return nil })

	data := storeRead(t, sf, "chat")
	if string(data) != "hello" {
		t.Errorf("Read(chat) = %q; want hello", data)
	}
}

func TestSessionFileStoreGetContent(t *testing.T) {
	sess := testSession("s1")
	defer sess.Cancel()
	sink := testSink()
	sf := store.NewSessionFileStore(sess, sink.NewLogger("test"),
		func() {}, func(string) error { return nil }, func([]byte) error { return nil })

	for _, name := range []string{"cfg", "offset", "usage", "ctxsz", "models", "systemprompt"} {
		if _, err := sf.Open(name); err != nil {
			t.Errorf("Get(%q): %v", name, err)
		}
	}
}

func TestSessionFileStorePutCwd(t *testing.T) {
	sess := testSession("s1")
	defer sess.Cancel()
	sink := testSink()
	sf := store.NewSessionFileStore(sess, sink.NewLogger("test"),
		func() {}, func(string) error { return nil }, func([]byte) error { return nil })

	storeWrite(t, sf, "cfg", []byte("cwd=/new/path"))
	core := sess.Core.(*stubCore)
	if core.cwd != "/new/path" {
		t.Errorf("cwd = %q; want /new/path", core.cwd)
	}
}

func TestSessionFileStorePutEmpty(t *testing.T) {
	sess := testSession("s1")
	defer sess.Cancel()
	sink := testSink()
	sf := store.NewSessionFileStore(sess, sink.NewLogger("test"),
		func() {}, func(string) error { return nil }, func([]byte) error { return nil })

	// Empty write is a no-op
	e, err := sf.Open("cfg")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := e.Write([]byte("")); err != nil {
		t.Fatalf("Write(spec, empty): %v", err)
	}
}

// ===== SessionFileStore: content, writeFile, handleCtl, blockingRead, makePublish =====

func TestSessionFileStoreContentAllFields(t *testing.T) {
	sess := testSession("s1")
	defer sess.Cancel()
	core := sess.Core.(*stubCore)
	core.usage = "100"
	core.ctxsz = "4096"
	core.models = "m1\nm2"
	core.sysprompt = "you are helpful"
	sess.ChatOffset = 5
	sf, _ := newSessionFileStore(t, sess)

	// individual metric files
	for _, tc := range []struct {
		name, want string
	}{
		{"usage", "100\n"},
		{"ctxsz", "4096\n"},
		{"models", "m1\nm2\n"},
		{"systemprompt", "you are helpful"},
		{"offset", "5\n"},
	} {
		data := storeRead(t, sf, tc.name)
		if string(data) != tc.want {
			t.Errorf("Read(%q) = %q; want %q", tc.name, data, tc.want)
		}
	}

	// spec contains all config and current state in KV form
	spec := string(storeRead(t, sf, "cfg"))
	for _, want := range []string{
		"state=idle\n", "backend=stub\n", "model=m\n",
		"agent=default\n", "cwd=/tmp\n",
	} {
		if !strings.Contains(spec, want) {
			t.Errorf("spec missing %q; got:\n%s", want, spec)
		}
	}
}

func TestSessionFileStoreReadFifoOut(t *testing.T) {
	sess := testSession("s1")
	defer sess.Cancel()
	core := sess.Core.(*stubCore)
	core.queued = []string{"queued-item"}
	sf, _ := newSessionFileStore(t, sess)

	data := storeRead(t, sf, "fifo.out")
	if string(data) != "queued-item" {
		t.Errorf("Read(fifo.out) = %q; want queued-item", data)
	}
	// Empty queue returns nil
	data2 := storeRead(t, sf, "fifo.out")
	if len(data2) != 0 {
		t.Errorf("Read(fifo.out) empty queue = %q; want empty", data2)
	}
}

func TestSessionFileStoreReadNotFound(t *testing.T) {
	sess := testSession("s1")
	defer sess.Cancel()
	sf, _ := newSessionFileStore(t, sess)

	if _, err := sf.Open("__bogus__"); err == nil {
		t.Error("Open(bogus) should error")
	}
}

func TestSessionFileStoreWritePrompt(t *testing.T) {
	sess := testSession("s1")
	defer sess.Cancel()
	sf, core := newSessionFileStore(t, sess)

	core.submitCh = make(chan struct{})
	storeWrite(t, sf, "prompt", []byte("hello agent"))
	<-core.submitCh
	if len(core.submitted) != 1 || core.submitted[0] != "hello agent" {
		t.Errorf("submitted = %v; want [hello agent]", core.submitted)
	}
}

func TestSessionFileStoreWriteFifoIn(t *testing.T) {
	sess := testSession("s1")
	defer sess.Cancel()
	sf, core := newSessionFileStore(t, sess)

	storeWrite(t, sf, "fifo.in", []byte("inject this"))
	if len(core.queued) != 1 || core.queued[0] != "inject this" {
		t.Errorf("queued = %v; want [inject this]", core.queued)
	}
}

func TestSessionFileStoreWriteChat(t *testing.T) {
	sess := testSession("s1")
	defer sess.Cancel()
	var saved []byte
	sf := newSessionFileStoreWith(t, sess,
		func() {}, func(string) error { return nil },
		func(data []byte) error { saved = data; return nil })

	storeWrite(t, sf, "chat", []byte("transcript data"))
	if string(saved) != "transcript data" {
		t.Errorf("saved = %q; want transcript data", saved)
	}
}

func TestSessionFileStoreWriteBackendModelAgent(t *testing.T) {
	sess := testSession("s1")
	defer sess.Cancel()
	sf, core := newSessionFileStore(t, sess)

	storeWrite(t, sf, "cfg", []byte("backend=openai"))
	if len(core.submitted) != 1 || core.submitted[0] != "/backend openai" {
		t.Errorf("submitted = %v; want [/backend openai]", core.submitted)
	}
	storeWrite(t, sf, "cfg", []byte("model=gpt-4"))
	if core.submitted[1] != "/model gpt-4" {
		t.Errorf("submitted[1] = %q; want /model gpt-4", core.submitted[1])
	}
	storeWrite(t, sf, "cfg", []byte("agent=coder"))
	if core.submitted[2] != "/agent coder" {
		t.Errorf("submitted[2] = %q; want /agent coder", core.submitted[2])
	}
}

func TestSessionFileStoreWriteBackendWhileRunning(t *testing.T) {
	sess := testSession("s1")
	defer sess.Cancel()
	core := sess.Core.(*stubCore)
	core.running = true
	sf, _ := newSessionFileStore(t, sess)

	e, _ := sf.Open("cfg")
	if err := e.Write([]byte("backend=openai")); err == nil {
		t.Error("Write spec backend= while running should error")
	}
	e2, _ := sf.Open("cfg")
	if err := e2.Write([]byte("model=gpt-4")); err == nil {
		t.Error("Write spec model= while running should error")
	}
	e3, _ := sf.Open("cfg")
	if err := e3.Write([]byte("agent=coder")); err == nil {
		t.Error("Write spec agent= while running should error")
	}
}

func TestSessionFileStoreWriteParams(t *testing.T) {
	sess := testSession("s1")
	defer sess.Cancel()
	sf, core := newSessionFileStore(t, sess)

	storeWrite(t, sf, "cfg", []byte("maxTokens=2048"))
	if core.params.MaxTokens != 2048 {
		t.Errorf("MaxTokens = %d; want 2048", core.params.MaxTokens)
	}
}

func TestSessionFileStoreWriteParamsWhileRunning(t *testing.T) {
	sess := testSession("s1")
	defer sess.Cancel()
	core := sess.Core.(*stubCore)
	core.running = true
	sf, _ := newSessionFileStore(t, sess)

	e, _ := sf.Open("cfg")
	if err := e.Write([]byte("maxTokens=2048")); err == nil {
		t.Error("Write spec maxTokens= while running should error")
	}
}

func TestSessionFileStoreHandleCtl(t *testing.T) {
	sess := testSession("s1")
	defer sess.Cancel()
	core := sess.Core.(*stubCore)
	core.running = true
	sf, _ := newSessionFileStore(t, sess)

	// stop
	storeWrite(t, sf, "ctl", []byte("stop"))
	if !core.interrupted {
		t.Error("ctl stop should interrupt")
	}

	// kill
	var killed bool
	sf2 := newSessionFileStoreWith(t, sess,
		func() { killed = true },
		func(string) error { return nil },
		func([]byte) error { return nil })
	storeWrite(t, sf2, "ctl", []byte("kill"))
	if !killed {
		t.Error("ctl kill should call kill callback")
	}

	// rn (rename)
	var renamed string
	sf3 := newSessionFileStoreWith(t, sess,
		func() {},
		func(id string) error { renamed = id; return nil },
		func([]byte) error { return nil })
	storeWrite(t, sf3, "ctl", []byte("rn newname"))
	if renamed != "newname" {
		t.Errorf("renamed = %q; want newname", renamed)
	}

	// save
	sess.AppendLog([]byte("log data"))
	var saved []byte
	sf4 := newSessionFileStoreWith(t, sess,
		func() {},
		func(string) error { return nil },
		func(data []byte) error { saved = data; return nil })
	storeWrite(t, sf4, "ctl", []byte("save"))
	if !strings.Contains(string(saved), "log data") {
		t.Errorf("saved = %q; want to contain log data", saved)
	}

	// slash commands forwarded to Submit
	core5 := &stubCore{state: "idle", backend: "stub", model: "m", agentName: "default", cwd: "/tmp"}
	ctx5, cancel5 := context.WithCancel(context.Background())
	defer cancel5()
	sess5 := store.NewSession("s5", core5, ctx5, cancel5)
	sf5, _ := newSessionFileStore(t, sess5)
	for _, cmd := range []string{"compact", "clear", "help", "history", "tools", "skills"} {
		storeWrite(t, sf5, "ctl", []byte(cmd))
	}
	for i, cmd := range []string{"compact", "clear", "help", "history", "tools", "skills"} {
		want := "/" + cmd
		if i >= len(core5.submitted) || core5.submitted[i] != want {
			t.Errorf("submitted[%d] = %q; want %q", i, core5.submitted[i], want)
		}
	}
}

func TestSessionFileStoreHandleCtlErrors(t *testing.T) {
	sess := testSession("s1")
	defer sess.Cancel()
	sf, _ := newSessionFileStore(t, sess)

	// unknown command
	e, _ := sf.Open("ctl")
	if err := e.Write([]byte("boguscmd")); err == nil {
		t.Error("unknown ctl command should error")
	}
}

func TestSessionFileStoreBlockingRead(t *testing.T) {
	core := &stubCore{state: "idle", backend: "stub", model: "m", agentName: "default", cwd: "/tmp", waitCh: make(chan string, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	sess := store.NewSession("s1", core, ctx, cancel)
	defer cancel()
	sf, _ := newSessionFileStore(t, sess)

	core.waitCh <- "running"

	e, err := sf.Open("statewait")
	if err != nil {
		t.Fatalf("Open(statewait): %v", err)
	}
	data, err := e.BlockingRead(context.Background(), "idle")
	if err != nil {
		t.Fatalf("BlockingRead: %v", err)
	}
	if string(data) != "running\n" {
		t.Errorf("BlockingRead = %q; want running\\n", data)
	}
}

func TestSessionFileStoreBlockingReadCancel(t *testing.T) {
	sess := testSession("s1")
	defer sess.Cancel()
	sf, _ := newSessionFileStore(t, sess)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	e, _ := sf.Open("statewait")
	data, err := e.BlockingRead(ctx, "idle")
	if err != nil {
		t.Fatalf("BlockingRead error: %v", err)
	}
	// On cancel/timeout, returns current state instead of nil
	if string(data) != "idle\n" {
		t.Errorf("BlockingRead cancelled = %q; want idle\\n", data)
	}
}

func TestSessionFileStoreBlockingReadNotWaitFile(t *testing.T) {
	sess := testSession("s1")
	defer sess.Cancel()
	sf, _ := newSessionFileStore(t, sess)

	e, _ := sf.Open("chat")
	if _, err := e.BlockingRead(context.Background(), ""); err == nil {
		t.Error("BlockingRead(chat) should error")
	}
}

func TestSessionFileStoreBlockingReadAllWaitFiles(t *testing.T) {
	// Start with state="thinking" so statewait blocks until state changes.
	// When base="" and state is already "idle", statewait returns immediately
	// to avoid blocking forever in wait loops.
	core := &stubCore{state: "thinking", backend: "stub", model: "m", agentName: "default", cwd: "/tmp", usage: "0", ctxsz: "0", waitCh: make(chan string, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	sess := store.NewSession("s1", core, ctx, cancel)
	defer cancel()
	sf, _ := newSessionFileStore(t, sess)

	for _, name := range []string{"statewait"} {
		core.waitCh <- "newval"
		e, err := sf.Open(name)
		if err != nil {
			t.Fatalf("Open(%q): %v", name, err)
		}
		data, err := e.BlockingRead(context.Background(), "")
		if err != nil {
			t.Fatalf("BlockingRead(%q): %v", name, err)
		}
		if string(data) != "newval\n" {
			t.Errorf("BlockingRead(%q) = %q; want newval\\n", name, data)
		}
	}
}

func TestSessionFileStoreBlockingReadIdleTimeout(t *testing.T) {
	// When state doesn't change, timeout returns current state
	core := &stubCore{state: "idle", backend: "stub", model: "m", agentName: "default", cwd: "/tmp"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	sess := store.NewSession("s1", core, ctx, cancel)
	sf, _ := newSessionFileStore(t, sess)

	e, err := sf.Open("statewait")
	if err != nil {
		t.Fatalf("Open(statewait): %v", err)
	}
	data, err := e.BlockingRead(ctx, "")
	if err != nil {
		t.Fatalf("BlockingRead: %v", err)
	}
	if string(data) != "idle\n" {
		t.Errorf("BlockingRead = %q; want idle\\n", data)
	}
}

func TestSessionFileStoreMakePublish(t *testing.T) {
	sess := testSession("s1")
	defer sess.Cancel()
	core := sess.Core.(*stubCore)
	core.reply = "hello back"
	sf, _ := newSessionFileStore(t, sess)

	storeWrite(t, sf, "prompt", []byte("hi"))
	time.Sleep(10 * time.Millisecond)

	l, _ := sess.LogInfo()
	if l == 0 {
		t.Error("session log should be non-empty after prompt+reply")
	}

	// Read the log to verify format
	data := storeRead(t, sf, "chat")
	if !strings.Contains(string(data), "assistant: hello back") {
		t.Errorf("chat log = %q; want to contain 'assistant: hello back'", data)
	}
}

func TestSessionFileStoreMakePublishMultipleEvents(t *testing.T) {
	// Manually exercise makePublish with varied event sequences
	core := &stubCore{state: "idle", backend: "stub", model: "m", agentName: "default", cwd: "/tmp"}
	// Override Submit to emit a sequence of events
	core2 := &publishCore{stubCore: core}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess := store.NewSession("s1", core2, ctx, cancel)
	sf := newSessionFileStoreWith(t, sess,
		func() {}, func(string) error { return nil }, func([]byte) error { return nil })

	storeWrite(t, sf, "prompt", []byte("test"))
	time.Sleep(10 * time.Millisecond)

	data := storeRead(t, sf, "chat")
	s := string(data)
	// Should contain user prefix, assistant prefix, tool call
	if !strings.Contains(s, "user: ") {
		t.Errorf("missing user prefix in %q", s)
	}
	if !strings.Contains(s, "assistant: ") {
		t.Errorf("missing assistant prefix in %q", s)
	}
	if !strings.Contains(s, "-> fn(") {
		t.Errorf("missing call in %q", s)
	}
}

func TestSessionFileStoreEntryStat(t *testing.T) {
	sess := testSession("s1")
	defer sess.Cancel()
	sess.AppendLog([]byte("hello"))
	sf, _ := newSessionFileStore(t, sess)

	e, _ := sf.Open("chat")
	fi, err := e.Stat()
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if fi.Size() != 5 {
		t.Errorf("entry Stat(chat).Size() = %d; want 5", fi.Size())
	}

	e2, _ := sf.Open("statewait")
	fi2, err := e2.Stat()
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	// Wait files report non-zero size so FUSE clients attempt to read
	if fi2.Size() == 0 {
		t.Error("entry Stat(statewait).Size() = 0; want non-zero for wait files")
	}

	e3, _ := sf.Open("cfg")
	fi3, err := e3.Stat()
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if fi3.Size() == 0 {
		t.Error("entry Stat(spec).Size() = 0; want non-zero")
	}
}

func TestSessionStoreOpenStore(t *testing.T) {
	s := newSessionStore(t)
	sess := testSession("s1")
	defer sess.Cancel()
	s.AddSession(sess)

	rs, err := s.OpenStore("s1")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	data := storeRead(t, rs, "cfg")
	if !strings.Contains(string(data), "state=idle\n") {
		t.Errorf("Read(state) = %q; want idle\\n", data)
	}
	if _, err := s.OpenStore("nope"); err == nil {
		t.Error("OpenStore(nonexistent) should error")
	}
}

// ===== LoadAgentConfig =====

func TestLoadAgentConfig(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "test.json"), []byte(`{}`), 0644)

	cfg := store.LoadAgentConfig(dir, "test", nil)
	if cfg == nil {
		t.Error("LoadAgentConfig should return non-nil for existing file")
	}

	cfg = store.LoadAgentConfig(dir, "nonexistent", nil)
	if cfg != nil {
		t.Error("LoadAgentConfig should return nil for missing file")
	}
}

// ===== FormatEvent =====

func TestFormatEvent(t *testing.T) {
	for _, tc := range []struct {
		ev   agent.Event
		want string
	}{
		{agent.Event{Role: "user", Content: "hi"}, "user: hi\n"},
		{agent.Event{Role: "assistant", Content: "hello"}, "hello"},
		{agent.Event{Role: "error", Content: "oops"}, "error: oops\n"},
		{agent.Event{Role: "call", Name: "fn", Content: "args"}, "-> fn(args)\n"},
		{agent.Event{Role: "tool", Content: "result\n"}, "result\n"},
		{agent.Event{Role: "retry", Content: "5"}, "retrying in 5s...\n"},
		{agent.Event{Role: "stalled"}, "agent stalled\n"},
		{agent.Event{Role: "info", Content: "note"}, ":: note"},
		{agent.Event{Role: "unknown"}, ""},
	} {
		got := string(store.FormatEvent(tc.ev))
		if got != tc.want {
			t.Errorf("FormatEvent(%q) = %q; want %q", tc.ev.Role, got, tc.want)
		}
	}
}

// ===== SyntheticFileInfo / SyntheticEntry / FileEntry / DirEntry =====

func TestSyntheticFileInfo(t *testing.T) {
	fi := &store.SyntheticFileInfo{Name_: "f", Mode_: 0644, Size_: 42, IsDir_: false}
	if fi.Name() != "f" || fi.Size() != 42 || fi.Mode() != 0644 || fi.IsDir() || fi.Sys() != nil {
		t.Error("SyntheticFileInfo field mismatch")
	}
	if !fi.ModTime().IsZero() {
		t.Error("ModTime should be zero")
	}
}

func TestFileEntryDirEntry(t *testing.T) {
	fe := store.FileEntry("f", 0644)
	if fe.Name() != "f" || fe.IsDir() || fe.Type() != 0 {
		t.Error("FileEntry mismatch")
	}
	de := store.DirEntry("d", 0755)
	if de.Name() != "d" || !de.IsDir() || de.Type() != os.ModeDir {
		t.Error("DirEntry mismatch")
	}
	// Info()
	info, err := de.Info()
	if err != nil || info.Name() != "d" || !info.IsDir() {
		t.Error("DirEntry.Info() mismatch")
	}
}

// ===== FormatParams / ParseParams =====

func TestFormatParamsRoundTrip(t *testing.T) {
	temp := 0.7
	freq := 0.1
	pres := 0.2
	p := backend.GenerationParams{
		MaxTokens:        1024,
		Temperature:      &temp,
		FrequencyPenalty: &freq,
		PresencePenalty:  &pres,
	}
	out := store.FormatParams(p)
	got, err := store.ParseParams(out, backend.GenerationParams{})
	if err != nil {
		t.Fatalf("ParseParams: %v", err)
	}
	if got.MaxTokens != 1024 {
		t.Errorf("MaxTokens = %d; want 1024", got.MaxTokens)
	}
	if got.Temperature == nil || *got.Temperature != 0.7 {
		t.Errorf("Temperature = %v; want 0.7", got.Temperature)
	}
	if got.FrequencyPenalty == nil || *got.FrequencyPenalty != 0.1 {
		t.Errorf("FrequencyPenalty = %v; want 0.1", got.FrequencyPenalty)
	}
	if got.PresencePenalty == nil || *got.PresencePenalty != 0.2 {
		t.Errorf("PresencePenalty = %v; want 0.2", got.PresencePenalty)
	}
}

func TestFormatParamsNilOptionals(t *testing.T) {
	p := backend.GenerationParams{MaxTokens: 512}
	out := store.FormatParams(p)
	got, err := store.ParseParams(out, backend.GenerationParams{})
	if err != nil {
		t.Fatalf("ParseParams: %v", err)
	}
	if got.MaxTokens != 512 {
		t.Errorf("MaxTokens = %d; want 512", got.MaxTokens)
	}
	if got.Temperature != nil {
		t.Errorf("Temperature should be nil, got %v", got.Temperature)
	}
}

func TestParseParamsClearWithEmpty(t *testing.T) {
	temp := 1.0
	p := backend.GenerationParams{MaxTokens: 100, Temperature: &temp}
	got, err := store.ParseParams("maxTokens=\ntemperature=\nfrequencyPenalty=\npresencePenalty=\n", p)
	if err != nil {
		t.Fatalf("ParseParams: %v", err)
	}
	if got.MaxTokens != 0 {
		t.Errorf("MaxTokens = %d; want 0", got.MaxTokens)
	}
	if got.Temperature != nil {
		t.Errorf("Temperature should be nil")
	}
}

func TestParseParamsErrors(t *testing.T) {
	if _, err := store.ParseParams("maxTokens=bad", backend.GenerationParams{}); err == nil {
		t.Error("expected error for invalid maxTokens")
	}
	if _, err := store.ParseParams("temperature=bad", backend.GenerationParams{}); err == nil {
		t.Error("expected error for invalid temperature")
	}
	if _, err := store.ParseParams("frequencyPenalty=bad", backend.GenerationParams{}); err == nil {
		t.Error("expected error for invalid frequencyPenalty")
	}
	if _, err := store.ParseParams("presencePenalty=bad", backend.GenerationParams{}); err == nil {
		t.Error("expected error for invalid presencePenalty")
	}
}

// ===== Session.Interrupt =====

func TestSessionInterrupt(t *testing.T) {
	sess := testSession("i1")
	defer sess.Cancel()
	sess.Interrupt()
	// stubCore.Interrupt is a no-op; just ensure it doesn't panic
}

// ===== SessionStore.createSession =====

func TestSessionStoreCreateSessionViaWrite(t *testing.T) {
	s := newSessionStoreWithCore(t)
	e, err := s.Open("new")
	if err != nil {
		t.Fatalf("Open(new): %v", err)
	}
	if err := e.Write([]byte("name=testsess cwd=/tmp agent=default")); err != nil {
		t.Fatalf("Write(new): %v", err)
	}
	if s.Session("testsess") == nil {
		t.Error("session 'testsess' not found after create")
	}
	s.KillSession("testsess")
}

func TestSessionStoreCreateSessionNoCwd(t *testing.T) {
	s := newSessionStoreWithCore(t)
	e, _ := s.Open("new")
	if err := e.Write([]byte("name=nocwd")); err == nil {
		t.Error("expected error for missing cwd")
	}
}

func TestSessionStoreCreateSessionDuplicate(t *testing.T) {
	s := newSessionStoreWithCore(t)
	e, _ := s.Open("new")
	e.Write([]byte("name=dup cwd=/tmp")) //nolint:errcheck
	if err := e.Write([]byte("name=dup cwd=/tmp")); err == nil {
		t.Error("expected error for duplicate session name")
	}
	s.KillSession("dup")
}

func TestSessionStoreCreateSessionBadOption(t *testing.T) {
	s := newSessionStoreWithCore(t)
	e, _ := s.Open("new")
	if err := e.Write([]byte("bogus cwd=/tmp")); err == nil {
		t.Error("expected error for invalid option")
	}
}

func TestSessionStoreCreateSessionUnknownKey(t *testing.T) {
	s := newSessionStoreWithCore(t)
	e, _ := s.Open("new")
	if err := e.Write([]byte("unknown=x cwd=/tmp")); err == nil {
		t.Error("expected error for unknown key")
	}
}

// ===== openEntry not-found =====

func TestSessionStoreOpenEntryNotFound(t *testing.T) {
	s := newSessionStore(t)
	if _, err := s.Open("__nonexistent__"); err == nil {
		t.Error("Open(nonexistent) should error")
	}
}

// ===== OpenStore not-found =====

func TestSessionStoreOpenStoreNotFound(t *testing.T) {
	s := newSessionStore(t)
	if _, err := s.OpenStore("__missing__"); err == nil {
		t.Error("OpenStore(missing) should error")
	}
}

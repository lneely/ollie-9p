package p9

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"ollie/pkg/agent"
	"ollie/pkg/backend"
	"ollie/pkg/tools"
	"ollie/pkg/tools/execute"
)

const batchNewTemplate = "name=\ncwd=\nagent=\nbackend=\nmodel=\noutput=\nparallel=1\n---\nWrite your prompt here.\n"

// batchJob holds state for one ephemeral batch agent run.
type batchJob struct {
	mu        sync.RWMutex
	id        string
	spec      string // verbatim input to b/new
	prompt    string
	cwd       string
	agentName string
	backend   string
	model     string
	output    string
	state     string // "running" | "done" | "failed: ..."
	result    string
	usage     string
	ctxsz     string
	log       []byte
	logVers   uint32
	cancel    context.CancelFunc
	done      chan struct{} // closed when state reaches a terminal value
}

// appendLog appends data to the job's log and bumps the Qid version.
func (job *batchJob) appendLog(data []byte) {
	if len(data) == 0 {
		return
	}
	job.mu.Lock()
	job.log = append(job.log, data...)
	job.logVers++
	job.mu.Unlock()
}

// batchSpec is the parsed result of a b/new write.
type batchSpec struct {
	name      string
	cwd       string
	agentName string
	backend   string
	model     string
	output    string
	parallel  int
	prompt    string
}

// BatchStore implements Store for the /b/ directory.
// Entries are job IDs (directories) plus the synthetic files "new" and "idx".
// Put("new", data) creates jobs; Delete(id) cancels and removes one.
type BatchStore struct {
	srv *Server
	mu  sync.RWMutex
	jobs map[string]*batchJob
}

func NewBatchStore(srv *Server) *BatchStore {
	return &BatchStore{srv: srv, jobs: make(map[string]*batchJob)}
}

func (s *BatchStore) List() ([]os.DirEntry, error) {
	entries := []os.DirEntry{
		syntheticEntry("new", 0666),
		syntheticEntry("idx", 0444),
	}
	s.mu.RLock()
	for id := range s.jobs {
		entries = append(entries, syntheticDirEntry(id, 0555))
	}
	s.mu.RUnlock()
	return entries, nil
}

func (s *BatchStore) Stat(name string) (os.FileInfo, error) {
	switch name {
	case "new":
		return &syntheticFileInfo{name: "new", mode: 0666, size: int64(len(batchNewTemplate))}, nil
	case "idx":
		return &syntheticFileInfo{name: "idx", mode: 0444, size: int64(len(s.index()))}, nil
	}
	s.mu.RLock()
	_, ok := s.jobs[name]
	s.mu.RUnlock()
	if ok {
		return &syntheticFileInfo{name: name, mode: 0555, isDir: true}, nil
	}
	return nil, fmt.Errorf("%s: not found", name)
}

func (s *BatchStore) Get(name string) ([]byte, error) {
	switch name {
	case "new":
		return []byte(batchNewTemplate), nil
	case "idx":
		return s.index(), nil
	}
	return nil, fmt.Errorf("%s: not a readable file", name)
}

func (s *BatchStore) Put(name string, data []byte) error {
	if name != "new" {
		return fmt.Errorf("%s: not writable", name)
	}
	return s.handleNewBatch(strings.TrimSpace(string(data)))
}

func (s *BatchStore) Delete(name string) error {
	s.mu.Lock()
	job, ok := s.jobs[name]
	if ok {
		delete(s.jobs, name)
	}
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("batch job not found: %s", name)
	}
	job.mu.RLock()
	cancel := job.cancel
	job.mu.RUnlock()
	if cancel != nil {
		cancel()
	}
	s.srv.log.Info("removed batch job %s", name)
	return nil
}

func (s *BatchStore) Create(name string) error {
	return fmt.Errorf("create not supported for batch jobs")
}

func (s *BatchStore) Rename(oldName, newName string) error {
	return fmt.Errorf("rename not supported for batch jobs")
}

// Shutdown cancels all running jobs.
func (s *BatchStore) Shutdown() {
	s.mu.Lock()
	ids := make([]string, 0, len(s.jobs))
	for id := range s.jobs {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.Delete(id) //nolint:errcheck
	}
}

// job looks up a job by ID (nil if not found).
func (s *BatchStore) job(id string) *batchJob {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.jobs[id]
}

// index returns the live b/idx content: id\tstate\tcwd\tagent per line.
func (s *BatchStore) index() []byte {
	var sb strings.Builder
	s.mu.RLock()
	defer s.mu.RUnlock()
	for id, job := range s.jobs {
		job.mu.RLock()
		fmt.Fprintf(&sb, "%s\t%s\t%s\t%s\n", id, job.state, job.cwd, job.agentName)
		job.mu.RUnlock()
	}
	return []byte(sb.String())
}

// handleNewBatch parses the spec, creates parallel batchJobs, and starts them.
func (s *BatchStore) handleNewBatch(input string) error {
	spec, err := parseBatchSpec(input)
	if err != nil {
		return err
	}

	baseID := spec.name
	if baseID == "" {
		baseID = agent.NewSessionID()
	}

	for i := 0; i < spec.parallel; i++ {
		id := fmt.Sprintf("%s-%d", baseID, i)

		s.mu.Lock()
		if _, exists := s.jobs[id]; exists {
			s.mu.Unlock()
			return fmt.Errorf("batch job already exists: %s", id)
		}
		job := &batchJob{
			id:        id,
			spec:      input,
			prompt:    spec.prompt,
			cwd:       spec.cwd,
			agentName: spec.agentName,
			backend:   spec.backend,
			model:     spec.model,
			output:    spec.output,
			state:     "running",
			done:      make(chan struct{}),
		}
		s.jobs[id] = job
		s.mu.Unlock()

		ctx, cancel := context.WithCancel(context.Background())
		job.mu.Lock()
		job.cancel = cancel
		job.mu.Unlock()

		go s.runJob(ctx, job)
	}

	s.srv.log.Info("new batch base=%s parallel=%d", baseID, spec.parallel)
	return nil
}

// runJob executes one batch job and updates its state and result.
func (s *BatchStore) runJob(ctx context.Context, job *batchJob) {
	result, err := s.executeJob(ctx, job)
	job.mu.Lock()
	defer job.mu.Unlock()
	if err != nil {
		if ctx.Err() != nil {
			job.state = "failed: cancelled"
		} else {
			job.state = "failed: " + err.Error()
		}
		job.result = job.state
		s.srv.log.Info("batch job %s failed: %v", job.id, err)
		close(job.done)
		return
	}
	job.result = result
	job.state = "done"
	s.srv.log.Info("batch job %s done", job.id)
	close(job.done)
}

// executeJob creates an ephemeral agentCore, submits the prompt, and returns
// the assistant reply. The core is closed before returning.
func (s *BatchStore) executeJob(ctx context.Context, job *batchJob) (string, error) {
	job.mu.RLock()
	backendName := job.backend
	modelName := job.model
	agentName := job.agentName
	cwd := job.cwd
	prompt := job.prompt
	output := job.output
	jobID := job.id
	job.mu.RUnlock()

	var (
		be  backend.Backend
		err error
	)
	if backendName != "" {
		old := os.Getenv("OLLIE_BACKEND")
		os.Setenv("OLLIE_BACKEND", backendName) //nolint:errcheck
		be, err = backend.New()
		os.Setenv("OLLIE_BACKEND", old) //nolint:errcheck
	} else {
		be, err = backend.New()
	}
	if err != nil {
		return "", fmt.Errorf("backend: %w", err)
	}
	if modelName != "" {
		be.SetModel(modelName)
	}

	cfg := loadAgentConfig(s.srv.agentsDir, agentName)

	newDisp := tools.NewDispatcherFunc(map[string]func() tools.Server{
		"execute": execute.Decl(cwd),
	})
	env := agent.BuildAgentEnv(cfg, newDisp(), cwd)

	core := agent.NewAgentCore(agent.AgentCoreConfig{
		Backend:       be,
		AgentName:     agentName,
		AgentsDir:     s.srv.agentsDir,
		SessionsDir:   "", // ephemeral: no persistence
		SessionID:     jobID,
		CWD:           cwd,
		Env:           env,
		NewDispatcher: newDisp,
		Log:           s.srv.sink.NewLogger("core"),
	})
	defer core.Close()

	if output == "json" {
		prompt += "\n\nRespond with valid JSON only, no prose."
	}

	var replyBuf, errBuf strings.Builder
	assistantStarted := false
	core.Submit(ctx, prompt, func(ev agent.Event) {
		switch ev.Role {
		case "assistant":
			replyBuf.WriteString(ev.Content)
			if !assistantStarted {
				job.appendLog([]byte("assistant: "))
				assistantStarted = true
			}
		case "user", "call", "tool":
			assistantStarted = false
		case "error":
			errBuf.WriteString(ev.Content)
		}
		job.appendLog(formatEvent(ev))
	})

	usage := core.Usage()
	ctxsz := core.CtxSz()

	job.mu.Lock()
	job.usage = usage
	job.ctxsz = ctxsz
	job.mu.Unlock()

	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if errBuf.Len() > 0 {
		return "", fmt.Errorf("%s", strings.TrimSpace(errBuf.String()))
	}
	return replyBuf.String(), nil
}

// parseBatchSpec splits input on the first \n---\n, parses key=value headers
// above it, and treats everything below as the prompt body.
func parseBatchSpec(input string) (batchSpec, error) {
	spec := batchSpec{
		agentName: "default",
		parallel:  1,
	}

	const sep = "\n---\n"
	idx := strings.Index(input, sep)
	if idx < 0 {
		return spec, fmt.Errorf("missing --- delimiter between headers and prompt")
	}

	for _, line := range strings.Split(input[:idx], "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		switch k {
		case "name":
			spec.name = v
		case "cwd":
			spec.cwd = v
		case "agent":
			spec.agentName = v
		case "backend":
			spec.backend = v
		case "model":
			spec.model = v
		case "output":
			spec.output = v
		case "parallel":
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				spec.parallel = n
			}
		}
	}

	spec.prompt = strings.TrimSpace(input[idx+len(sep):])

	if spec.cwd == "" {
		return spec, fmt.Errorf("cwd is required")
	}
	if spec.prompt == "" {
		return spec, fmt.Errorf("prompt is required")
	}
	return spec, nil
}

// ---------------------------------------------------------------------------
// BatchJobStore — per-job file access, mirrors SessionFileStore.
// ---------------------------------------------------------------------------

var batchJobFiles = []struct {
	name string
	mode os.FileMode
}{
	{"spec",      0444},
	{"state",     0444},
	{"statewait", 0444},
	{"result",    0444},
	{"usage",     0444},
	{"ctxsz",     0444},
	{"log",       0444},
}

// BatchJobStore provides Stat/List/Get for the files within a single batch job
// directory (/b/{id}/*). The file set is fixed.
type BatchJobStore struct {
	job *batchJob
}

func (s *BatchStore) JobStore(id string) (*BatchJobStore, bool) {
	job := s.job(id)
	if job == nil {
		return nil, false
	}
	return &BatchJobStore{job: job}, true
}

func (js *BatchJobStore) List() ([]os.DirEntry, error) {
	entries := make([]os.DirEntry, len(batchJobFiles))
	for i, f := range batchJobFiles {
		entries[i] = syntheticEntry(f.name, f.mode)
	}
	return entries, nil
}

func (js *BatchJobStore) Stat(name string) (os.FileInfo, error) {
	for _, f := range batchJobFiles {
		if f.name == name {
			var size int64
			if name == "log" {
				js.job.mu.RLock()
				size = int64(len(js.job.log))
				js.job.mu.RUnlock()
			} else {
				size = int64(len(js.content(name)))
			}
			return &syntheticFileInfo{name: name, mode: f.mode, size: size}, nil
		}
	}
	return nil, fmt.Errorf("%s: not found", name)
}

func (js *BatchJobStore) Get(name string) ([]byte, error) {
	if name == "log" {
		js.job.mu.RLock()
		data := make([]byte, len(js.job.log))
		copy(data, js.job.log)
		js.job.mu.RUnlock()
		return data, nil
	}
	for _, f := range batchJobFiles {
		if f.name == name {
			return []byte(js.content(name)), nil
		}
	}
	return nil, fmt.Errorf("%s: not found", name)
}

func (js *BatchJobStore) content(name string) string {
	js.job.mu.RLock()
	defer js.job.mu.RUnlock()
	switch name {
	case "spec":
		return js.job.spec
	case "state":
		return js.job.state + "\n"
	case "result":
		return js.job.result
	case "usage":
		return js.job.usage + "\n"
	case "ctxsz":
		return js.job.ctxsz + "\n"
	}
	return ""
}

// Wait blocks until the job reaches a terminal state, then returns it.
// Returns nil content (empty read) on context cancellation.
func (js *BatchJobStore) Wait(ctx context.Context, name, _ string) ([]byte, error) {
	if name != "statewait" {
		return nil, fmt.Errorf("%s: not a wait file", name)
	}
	select {
	case <-ctx.Done():
		return nil, nil
	case <-js.job.done:
	}
	js.job.mu.RLock()
	state := js.job.state
	js.job.mu.RUnlock()
	return []byte(state + "\n"), nil
}

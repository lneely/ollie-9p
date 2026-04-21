package p9

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"ollie/pkg/agent"
	"ollie/pkg/backend"
	olog "ollie/pkg/log"
)

// sessionFileList defines the fixed set of files in a session directory,
// with their 9P permission modes.
var sessionFileList = []struct {
	name string
	mode os.FileMode
}{
	{"ctl", 0200},
	{"prompt", 0200},
	{"fifo.in", 0200},
	{"fifo.out", 0444},
	{"chat", 0666},
	{"offset", 0444},
	{"state", 0444},
	{"statewait", 0444},
	{"backend", 0666},
	{"agent", 0666},
	{"model", 0666},
	{"cwd", 0666},
	{"cwdwait", 0444},
	{"usage", 0444},
	{"usagewait", 0444},
	{"ctxsz", 0444},
	{"ctxszwait", 0444},
	{"models", 0444},
	{"systemprompt", 0444},
	{"params", 0666},
}

// SessionFileStore implements ReadWriteStore for the files within a single
// session directory (/s/{id}/*). The file set is fixed; Create, Delete, and
// Rename are not meaningful and are not part of the interface.
type SessionFileStore struct {
	sess           *session
	log            *olog.Logger
	kill           func()
	rename         func(newID string) error
	saveTranscript func([]byte) error
}

func NewSessionFileStore(sess *session, log *olog.Logger, kill func(), rename func(newID string) error, saveTranscript func([]byte) error) *SessionFileStore {
	return &SessionFileStore{sess: sess, log: log, kill: kill, rename: rename, saveTranscript: saveTranscript}
}

func (s *SessionFileStore) List() ([]os.DirEntry, error) {
	entries := make([]os.DirEntry, len(sessionFileList))
	for i, f := range sessionFileList {
		entries[i] = syntheticEntry(f.name, f.mode)
	}
	return entries, nil
}

func (s *SessionFileStore) Stat(name string) (os.FileInfo, error) {
	for _, f := range sessionFileList {
		if f.name == name {
			var size int64
			switch name {
			case "chat":
				s.sess.mu.RLock()
				size = int64(len(s.sess.chatLog))
				s.sess.mu.RUnlock()
			case "statewait", "usagewait", "ctxszwait", "cwdwait":
				// Blocking reads; size is unknown until resolved.
			default:
				size = int64(len(s.content(name)))
			}
			return &syntheticFileInfo{Name_: name, Mode_: f.mode, Size_: size}, nil
		}
	}
	return nil, fmt.Errorf("%s: not found", name)
}

func (s *SessionFileStore) Get(name string) ([]byte, error) {
	switch name {
	case "chat":
		s.sess.mu.RLock()
		data := make([]byte, len(s.sess.chatLog))
		copy(data, s.sess.chatLog)
		s.sess.mu.RUnlock()
		return data, nil
	case "offset":
		return []byte(s.content("offset")), nil
	case "fifo.out":
		item, ok := s.sess.core.PopQueue()
		if !ok {
			return nil, nil
		}
		return []byte(item), nil
	default:
		for _, f := range sessionFileList {
			if f.name == name {
				return []byte(s.content(name)), nil
			}
		}
		return nil, fmt.Errorf("%s: not found", name)
	}
}

func (s *SessionFileStore) Put(name string, data []byte) error {
	input := strings.TrimSpace(string(data))
	if input == "" {
		return nil
	}
	switch name {
	case "chat":
		return s.saveTranscript([]byte(input))

	case "prompt":
		s.sess.core.Submit(s.sess.ctx, input, s.makePublish())

	case "fifo.in":
		s.sess.core.Queue(input)

	case "ctl":
		return s.handleCtl(input)

	case "backend":
		if s.sess.core.IsRunning() {
			return fmt.Errorf("cannot switch backend while agent is running")
		}
		s.sess.core.Submit(s.sess.ctx, "/backend "+input, s.makePublish())

	case "agent":
		if s.sess.core.IsRunning() {
			return fmt.Errorf("cannot switch agent while agent is running")
		}
		s.sess.core.Submit(s.sess.ctx, "/agent "+input, s.makePublish())

	case "model":
		if s.sess.core.IsRunning() {
			return fmt.Errorf("cannot switch model while agent is running")
		}
		s.sess.core.Submit(s.sess.ctx, "/model "+input, s.makePublish())

	case "cwd":
		if err := s.sess.core.SetCWD(input); err != nil {
			return err
		}

	case "params":
		if s.sess.core.IsRunning() {
			return fmt.Errorf("cannot change params while agent is running")
		}
		params, err := parseParams(input, s.sess.core.GenerationParams())
		if err != nil {
			return err
		}
		return s.sess.core.SetGenerationParams(params)

	}
	return nil
}

func formatParams(p backend.GenerationParams) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "maxTokens=%d\n", p.MaxTokens)
	if p.Temperature != nil {
		fmt.Fprintf(&sb, "temperature=%g\n", *p.Temperature)
	} else {
		fmt.Fprintf(&sb, "temperature=\n")
	}
	if p.FrequencyPenalty != nil {
		fmt.Fprintf(&sb, "frequencyPenalty=%g\n", *p.FrequencyPenalty)
	} else {
		fmt.Fprintf(&sb, "frequencyPenalty=\n")
	}
	if p.PresencePenalty != nil {
		fmt.Fprintf(&sb, "presencePenalty=%g\n", *p.PresencePenalty)
	} else {
		fmt.Fprintf(&sb, "presencePenalty=\n")
	}
	return sb.String()
}

func parseParams(input string, current backend.GenerationParams) (backend.GenerationParams, error) {
	p := current
	for _, line := range strings.Split(input, "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		switch k {
		case "maxTokens":
			if v == "" {
				p.MaxTokens = 0
			} else {
				n, err := strconv.Atoi(v)
				if err != nil {
					return p, fmt.Errorf("invalid maxTokens: %s", v)
				}
				p.MaxTokens = n
			}
		case "temperature":
			if v == "" {
				p.Temperature = nil
			} else {
				f, err := strconv.ParseFloat(v, 64)
				if err != nil {
					return p, fmt.Errorf("invalid temperature: %s", v)
				}
				p.Temperature = &f
			}
		case "frequencyPenalty":
			if v == "" {
				p.FrequencyPenalty = nil
			} else {
				f, err := strconv.ParseFloat(v, 64)
				if err != nil {
					return p, fmt.Errorf("invalid frequencyPenalty: %s", v)
				}
				p.FrequencyPenalty = &f
			}
		case "presencePenalty":
			if v == "" {
				p.PresencePenalty = nil
			} else {
				f, err := strconv.ParseFloat(v, 64)
				if err != nil {
					return p, fmt.Errorf("invalid presencePenalty: %s", v)
				}
				p.PresencePenalty = &f
			}
		}
	}
	return p, nil
}

// content returns the string content of a simple readable session file.
func (s *SessionFileStore) content(name string) string {
	s.sess.mu.RLock()
	defer s.sess.mu.RUnlock()
	switch name {
	case "backend":
		return s.sess.core.BackendName() + "\n"
	case "agent":
		return s.sess.core.AgentName() + "\n"
	case "model":
		return s.sess.core.ModelName() + "\n"
	case "state":
		return s.sess.core.State() + "\n"
	case "cwd":
		return s.sess.core.CWD() + "\n"
	case "usage":
		return s.sess.core.Usage() + "\n"
	case "ctxsz":
		return s.sess.core.CtxSz() + "\n"
	case "models":
		return s.sess.core.ListModels() + "\n"
	case "systemprompt":
		return s.sess.core.SystemPrompt()
	case "offset":
		return fmt.Sprintf("%d\n", s.sess.chatOffset)
	case "params":
		return formatParams(s.sess.core.GenerationParams())
	}
	return ""
}

// CurrentWaitValue returns the current value for the named *wait file.
func (s *SessionFileStore) CurrentWaitValue(name string) string {
	switch name {
	case "statewait":
		return s.sess.core.State()
	case "usagewait":
		return s.sess.core.Usage()
	case "ctxszwait":
		return s.sess.core.CtxSz()
	case "cwdwait":
		return s.sess.core.CWD()
	}
	return ""
}

// Wait blocks until the named *wait file's underlying value changes from base,
// then returns the new value. If base is empty, the current value is used.
// Unblocks when connCtx or the session context is cancelled.
func (s *SessionFileStore) Wait(connCtx context.Context, name, base string) ([]byte, error) {
	ctx, cancel := context.WithCancel(connCtx)
	defer cancel()
	// also unblock when the session itself is killed.
	context.AfterFunc(s.sess.ctx, cancel)

	var field string
	switch name {
	case "statewait":
		field = agent.WatchState
	case "usagewait":
		field = agent.WatchUsage
	case "ctxszwait":
		field = agent.WatchCtxSz
	case "cwdwait":
		field = agent.WatchCWD
	default:
		return nil, fmt.Errorf("%s: not a wait file", name)
	}

	if base == "" {
		base = s.CurrentWaitValue(name)
	}

	v, ok := s.sess.core.WaitChange(ctx, field, base)
	if !ok {
		return nil, nil
	}
	return []byte(v + "\n"), nil
}

func (s *SessionFileStore) makePublish() func(agent.Event) {
	assistantStarted := false
	return func(ev agent.Event) {
		switch ev.Role {
		case "user", "call", "tool":
			assistantStarted = false
		case "assistant":
			if !assistantStarted {
				s.sess.appendChat([]byte("assistant: "))
				assistantStarted = true
			}
		}
		s.sess.appendChat(formatEvent(ev))
		if ev.Role == "user" {
			s.sess.mu.Lock()
			s.sess.chatOffset = len(s.sess.chatLog)
			s.sess.mu.Unlock()
		}
	}
}

func (s *SessionFileStore) handleCtl(input string) error {
	cmd := strings.Fields(input)
	if len(cmd) == 0 {
		return fmt.Errorf("empty ctl command")
	}
	switch cmd[0] {
	case "stop":
		s.sess.core.Interrupt(agent.ErrInterrupted)
	case "kill":
		s.kill()
	case "rn":
		if name := strings.TrimSpace(input[3:]); name != "" {
			if err := s.rename(name); err != nil {
				s.log.Error("rename: %v", err)
			}
		}
	case "save":
		s.sess.mu.RLock()
		data := make([]byte, len(s.sess.chatLog))
		copy(data, s.sess.chatLog)
		s.sess.mu.RUnlock()
		return s.saveTranscript(data)
	case "compact", "clear", "backend", "model", "models",
		"agents", "agent", "sessions", "cwd", "skills",
		"tools", "context", "usage", "history",
		"irw", "help":
		s.sess.core.Submit(s.sess.ctx, "/"+input, s.makePublish())
	default:
		return fmt.Errorf("unknown ctl command: %s", cmd[0])
	}
	return nil
}

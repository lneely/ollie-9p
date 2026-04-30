package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"ollie/pkg/agent"
	"ollie/pkg/backend"
	olog "ollie/pkg/log"
)

// SessionFileList defines the fixed set of files in a session directory.
var SessionFileList = []struct {
	Name string
	Mode os.FileMode
}{
	{"plan", 0666},
	{"ctl", 0200},
	{"prompt", 0200},
	{"fifo.in", 0200},
	{"fifo.out", 0444},
	{"chat", 0666},
	{"offset", 0444},
	{"cfg", 0666},
	{"statewait", 0444},
	{"usage", 0444},
	{"cost", 0444},
	{"ctxsz", 0444},
	{"models", 0444},
	{"systemprompt", 0444},
	{"env", 0444},
	{"tail", 0555},
	{"prompt.prev", 0444},
	{"context", 0444},
}

// SessionFileStore is a RunFileStore for a session directory.
type SessionFileStore = RunFileStore

func NewSessionFileStore(sess *Session, log *olog.Logger, kill func(), rename func(newID string) error, saveTranscript func([]byte) error) *SessionFileStore {
	h := &sessionHelper{sess: sess, log: log, kill: kill, rename: rename, saveTranscript: saveTranscript}
	specs := make([]FileSpec, len(SessionFileList))
	for i, f := range SessionFileList {
		specs[i] = h.fileSpec(f.Name, f.Mode)
	}
	return NewRunFileStore(sess, specs)
}

// sessionHelper holds the dependencies needed to build session FileSpecs.
type sessionHelper struct {
	sess           *Session
	log            *olog.Logger
	kill           func()
	rename         func(newID string) error
	saveTranscript func([]byte) error
}

func (h *sessionHelper) fileSpec(name string, mode os.FileMode) FileSpec {
	fs := FileSpec{Name: name, Mode: mode}

	// Read
	switch name {
	case "plan":
		fs.Read = func() ([]byte, error) {
			h.sess.mu.RLock()
			data := make([]byte, len(h.sess.plan))
			copy(data, h.sess.plan)
			h.sess.mu.RUnlock()
			return data, nil
		}
		fs.Size = func() int64 {
			h.sess.mu.RLock()
			n := len(h.sess.plan)
			h.sess.mu.RUnlock()
			return int64(n)
		}
	case "prompt.prev":
		fs.Read = func() ([]byte, error) {
			h.sess.mu.RLock()
			data := make([]byte, len(h.sess.prevPrompt))
			copy(data, h.sess.prevPrompt)
			h.sess.mu.RUnlock()
			return data, nil
		}
	case "chat":
		fs.Read = func() ([]byte, error) {
			h.sess.mu.RLock()
			data := make([]byte, len(h.sess.log))
			copy(data, h.sess.log)
			h.sess.mu.RUnlock()
			return data, nil
		}
		fs.Size = func() int64 {
			h.sess.mu.RLock()
			n := len(h.sess.log)
			h.sess.mu.RUnlock()
			return int64(n)
		}
	case "fifo.out":
		fs.Read = func() ([]byte, error) {
			item, ok := h.sess.Core.PopQueue()
			if !ok {
				return nil, nil
			}
			return []byte(item), nil
		}
	case "cfg":
		fs.Read = func() ([]byte, error) { return []byte(h.cfgContent()), nil }
	default:
		fs.Read = func() ([]byte, error) { return []byte(h.content(name)), nil }
	}

	// Write
	switch name {
	case "plan":
		fs.Write = func(data []byte) error {
			h.sess.mu.Lock()
			h.sess.plan = make([]byte, len(data))
			copy(h.sess.plan, data)
			h.sess.mu.Unlock()
			return nil
		}
	case "chat":
		fs.Write = func(data []byte) error {
			input := strings.TrimSpace(string(data))
			if input == "" {
				return nil
			}
			return h.saveTranscript([]byte(input))
		}
	case "prompt":
		fs.Write = func(data []byte) error {
			input := strings.TrimSpace(string(data))
			if input == "" {
				return nil
			}
			h.sess.mu.Lock()
			h.sess.prevPrompt = []byte(input)
			h.sess.mu.Unlock()
			pub := h.makePublish()
			go func() {
				h.sess.Core.Submit(h.sess.Ctx, input, pub)
				h.sess.EnsureTrailingNewline()
			}()
			return nil
		}
	case "fifo.in":
		fs.Write = func(data []byte) error {
			input := strings.TrimSpace(string(data))
			if input == "" {
				return nil
			}
			h.sess.Core.Queue(input)
			return nil
		}
	case "ctl":
		fs.Write = func(data []byte) error {
			input := strings.TrimSpace(string(data))
			if input == "" {
				return nil
			}
			return h.handleCtl(input)
		}
	case "cfg":
		fs.Write = func(data []byte) error {
			input := strings.TrimSpace(string(data))
			if input == "" {
				return nil
			}
			return h.handleCfg(input)
		}
	}

	// Wait
	if name == "statewait" {
		fs.Wait = func(connCtx context.Context, base string) ([]byte, error) {
			ctx, cancel := context.WithCancel(connCtx)
			defer cancel()
			context.AfterFunc(h.sess.Ctx, cancel)
			if base == "" {
				base = h.sess.Core.State()
			}
			v, ok := h.sess.Core.WaitChange(ctx, agent.WatchState, base)
			if !ok {
				// Timeout or cancel: return current state so caller can check.
				return []byte(h.sess.Core.State() + "\n"), nil
			}
			return []byte(v + "\n"), nil
		}
	}

	return fs
}

func (h *sessionHelper) content(name string) string {
	h.sess.mu.RLock()
	defer h.sess.mu.RUnlock()
	switch name {
	case "usage":
		return h.sess.Core.Usage() + "\n"
	case "cost":
		return h.sess.Core.Cost()
	case "ctxsz":
		return h.sess.Core.CtxSz() + "\n"
	case "models":
		return h.sess.Core.ListModels() + "\n"
	case "systemprompt":
		return h.sess.Core.SystemPrompt()
	case "offset":
		return fmt.Sprintf("%d\n", h.sess.ChatOffset)
	case "env":
		return h.envContent()
	case "tail":
		return "#!/bin/sh\nexec tail -f \"$(dirname \"$0\")/chat\"\n"
	case "context":
		var sb strings.Builder
		enc := json.NewEncoder(&sb)
		enc.SetEscapeHTML(false)
		for _, m := range h.sess.Core.Context() {
			enc.Encode(m)
		}
		return sb.String()
	}
	return ""
}

func (h *sessionHelper) envContent() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "OLLIE_SESSION_ID=%s\n", h.sess.RunnableID())
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "OLLIE_") && !strings.HasPrefix(e, "OLLIE_SESSION_ID=") {
			sb.WriteString(e)
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}


func (h *sessionHelper) cfgContent() string {
	h.sess.mu.RLock()
	defer h.sess.mu.RUnlock()
	p := h.sess.Core.GenerationParams()
	var sb strings.Builder
	fmt.Fprintf(&sb, "state=%s\n", h.sess.Core.State())
	fmt.Fprintf(&sb, "backend=%s\n", h.sess.Core.BackendName())
	fmt.Fprintf(&sb, "model=%s\n", h.sess.Core.ModelName())
	fmt.Fprintf(&sb, "agent=%s\n", h.sess.Core.AgentName())
	fmt.Fprintf(&sb, "cwd=%s\n", h.sess.Core.CWD())
	fmt.Fprintf(&sb, "maxTokens=%d\n", p.MaxTokens)
	if p.Temperature != nil {
		fmt.Fprintf(&sb, "temperature=%g\n", *p.Temperature)
	} else {
		sb.WriteString("temperature=\n")
	}
	if p.FrequencyPenalty != nil {
		fmt.Fprintf(&sb, "frequencyPenalty=%g\n", *p.FrequencyPenalty)
	} else {
		sb.WriteString("frequencyPenalty=\n")
	}
	if p.PresencePenalty != nil {
		fmt.Fprintf(&sb, "presencePenalty=%g\n", *p.PresencePenalty)
	} else {
		sb.WriteString("presencePenalty=\n")
	}
	return sb.String()
}

func (h *sessionHelper) handleCfg(input string) error {
	p := h.sess.Core.GenerationParams()
	hasParams := false
	for _, line := range strings.Split(input, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "backend", "model", "agent":
			if v == "" {
				continue
			}
			if h.sess.Core.IsRunning() {
				return fmt.Errorf("cannot switch %s while agent is running", k)
			}
			h.sess.Core.Submit(h.sess.Ctx, "/"+k+" "+v, h.makePublish())
		case "cwd":
			if v == "" {
				continue
			}
			if err := h.sess.Core.SetCWD(v); err != nil {
				return err
			}
		case "maxTokens":
			hasParams = true
			if v == "" {
				p.MaxTokens = 0
			} else if n, err := strconv.Atoi(v); err == nil {
				p.MaxTokens = n
			}
		case "temperature":
			hasParams = true
			if v == "" {
				p.Temperature = nil
			} else if f, err := strconv.ParseFloat(v, 64); err == nil {
				p.Temperature = &f
			}
		case "frequencyPenalty":
			hasParams = true
			if v == "" {
				p.FrequencyPenalty = nil
			} else if f, err := strconv.ParseFloat(v, 64); err == nil {
				p.FrequencyPenalty = &f
			}
		case "presencePenalty":
			hasParams = true
			if v == "" {
				p.PresencePenalty = nil
			} else if f, err := strconv.ParseFloat(v, 64); err == nil {
				p.PresencePenalty = &f
			}
		// state → read-only, silently ignored
		}
	}
	if hasParams {
		if h.sess.Core.IsRunning() {
			return fmt.Errorf("cannot change params while agent is running")
		}
		return h.sess.Core.SetGenerationParams(p)
	}
	return nil
}


func (h *sessionHelper) makePublish() func(agent.Event) {
	assistantStarted := false
	return func(ev agent.Event) {
		if ev.Role == "user" {
			if assistantStarted {
				h.sess.AppendLog([]byte("\n"))
				assistantStarted = false
			}
			h.sess.AppendLog(FormatEvent(ev))
			return
		} else {
			switch ev.Role {
			case "assistant":
				if !assistantStarted {
					h.sess.AppendLog([]byte("assistant: "))
					h.sess.mu.Lock()
					h.sess.ChatOffset = len(h.sess.log)
					h.sess.mu.Unlock()
					assistantStarted = true
				}
			default:
				if assistantStarted {
					h.sess.AppendLog([]byte("\n"))
					assistantStarted = false
				}
			}
		}
		h.sess.AppendLog(FormatEvent(ev))
	}
}

func (h *sessionHelper) handleCtl(input string) error {
	cmd := strings.Fields(input)
	if len(cmd) == 0 {
		return fmt.Errorf("empty ctl command")
	}
	switch cmd[0] {
	case "stop":
		h.sess.Core.Interrupt(agent.ErrInterrupted)
	case "kill":
		h.kill()
	case "rn":
		if name := strings.TrimSpace(input[3:]); name != "" {
			if err := h.rename(name); err != nil {
				h.log.Error("rename: %v", err)
			}
		}
	case "save":
		h.sess.mu.RLock()
		data := make([]byte, len(h.sess.log))
		copy(data, h.sess.log)
		h.sess.mu.RUnlock()
		return h.saveTranscript(data)
	case "compact", "clear", "backend", "model", "models",
		"agents", "agent", "sessions", "cwd", "skills",
		"tools", "context", "usage", "cost", "history",
		"irw", "help":
		h.sess.Core.Submit(h.sess.Ctx, "/"+input, h.makePublish())
	default:
		return fmt.Errorf("unknown ctl command: %s", cmd[0])
	}
	return nil
}

// FormatParams formats generation parameters as key=value lines.
func FormatParams(p backend.GenerationParams) string {
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

// ParseParams parses key=value lines into generation parameters.
func ParseParams(input string, current backend.GenerationParams) (backend.GenerationParams, error) {
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

# olliesrv

A 9P server that exposes [ollie](../ollie) agent sessions as a virtual filesystem. Mount it with `9pfuse` and interact with AI sessions using ordinary shell tools.

The goal is integration, not self-sufficiency. Rather than providing orchestration, scheduling, or workflow primitives, olliesrv exposes a stable surface — sessions as directories, conversation as files — and defers everything else to the surrounding environment. Scripting, chaining, monitoring, and automation come from composing olliesrv with tools that already exist, not from building those capabilities into the server.

## Filesystem layout

```
ollie/
  a/                    dir:   agent configs (r/w, backed by ~/.config/ollie/agents/)
    <name>.json         r/w:   agent config; supports mv, cp, rm
  backends              read:  list of ollie-provided backends
  help                  read:  help file (backed by ~/.config/ollie/help.md)
  p/                    dir:   prompt templates (r/w, backed by ~/.config/ollie/prompts/)
    SYSTEM_PROMPT.md    r/w:   the system prompt template
  pl/                   dir:   plans
  s/                    dir:   one entry per active session, sorted by creation time
    new                 r/w:   read: KV template; write: create session
    <session-id>/               rm -r to kill session; mv to rename
      agent             r/w:   active agent name
      backend           r/w:   active backend name
      chat              read:  cumulative conversation history
      ctl               write: stop | <command>
      ctxsz             read:  estimated context size vs context window
      dequeue           read:  pop the next queued prompt
      enqueue           write: queue a prompt for later execution
      mcp               read:  MCP server list
      model             r/w:   active model name
      models            read:  available models from the backend
      prompt            write: submit a prompt to the agent
      reply             read:  assistant text from the most recent turn only
      state             read:  current agent state (idle, thinking, calling: <tool>)
      usage             read:  token counts (input, output, requests; [estimated] if not reported by backend)
      cwd               r/w:   working directory for tool execution and system prompt
  sk/                   dir:   skills (r/w, from OLLIE_SKILLS_PATH or ~/.config/ollie/skills/)
    <name>.md           r/w:   skill SKILL.md content
  t/                    dir:   tool scripts (r/w, backed by ~/.config/ollie/tools/)
    <script>            r/w:   tool script content
```

Session IDs are Unix nanosecond timestamps with a random suffix (e.g. `1744276689123456789-2b986c`), so `ls s/` sorted lexicographically gives creation order.

## Building

```sh
mk
```

Installs `olliesrv` to `$HOME/bin`.

## Usage

```sh
olliesrv start       # start daemon (backgrounds itself)
olliesrv fgstart     # start in foreground
olliesrv stop        # stop daemon
olliesrv status      # check if running
```

The server listens on a Unix socket in the Plan 9 namespace (`$NAMESPACE/ollie`) and optionally mounts via `9pfuse` to `$HOME/mnt/ollie` (or `$OLLIE_9MOUNT`).

## Sessions

### Create a session

```sh
cat ~/mnt/ollie/s/new                # show required/optional KV pairs
echo "cwd=/home/lkn/src/myproject" > ~/mnt/ollie/s/new
echo "cwd=/home/lkn/src/myproject backend=ollama model=qwen3:8b" > ~/mnt/ollie/s/new
```

Valid keys: `cwd` (required), `backend`, `model`, `agent`.

A new session directory appears under `s/`.

### Send a prompt

```sh
echo "what files are in the current directory?" > ~/mnt/ollie/s/<session-id>/prompt
```

Writes dispatch asynchronously on close, so the shell returns immediately. The agent runs in the background.

### Read the conversation

```sh
cat ~/mnt/ollie/s/<session-id>/chat             # full history snapshot
tail -f ~/mnt/ollie/s/<session-id>/chat         # follow output as it arrives
```

The `chat` file is an append-only log of the full conversation.

### Check agent state

```sh
cat ~/mnt/ollie/s/<session-id>/state
# idle | thinking | calling: <toolname>
```

### Control a session

```sh
echo stop > ~/mnt/ollie/s/<session-id>/ctl          # interrupt the current turn
echo compact > ~/mnt/ollie/s/<session-id>/ctl        # summarize context
echo clear > ~/mnt/ollie/s/<session-id>/ctl          # clear session history
echo kill > ~/mnt/ollie/s/<session-id>/ctl           # kill session
echo "rn my-name" > ~/mnt/ollie/s/<session-id>/ctl   # rename session
echo "model qwen3:8b" > ~/mnt/ollie/s/<session-id>/ctl
```

`ctl` accepts only recognized commands: `stop`, `kill`, `rn <name>`, `compact`, `clear`, `backend`, `model`, `models`, `agents`, `agent`, `sessions`, `cwd`, `skills`, `tools`, `mcp`, `context`, `usage`, `history`, `irw`, `help`. The `/` prefix is added automatically. Unrecognized input is rejected with an error.

### Switch backend, model, or agent

```sh
echo ollama > ~/mnt/ollie/s/<session-id>/backend
echo qwen3:8b > ~/mnt/ollie/s/<session-id>/model
echo myagent > ~/mnt/ollie/s/<session-id>/agent
```

Writes to `backend`, `model`, and `agent` are rejected with an error when the agent is not idle. Writes to `ctl` return an error for unrecognized commands; recognized commands are dispatched asynchronously. Check `state` to confirm the change took effect.

### Kill a session

```sh
rm -r ~/mnt/ollie/s/<session-id>
```

### Rename a session

```sh
mv ~/mnt/ollie/s/<session-id> ~/mnt/ollie/s/my-friendly-name
```

Rename is rejected if the agent is running or the target name already exists. All open file handles into the session are updated automatically.

## Agents

Agent configs live in `a/` and are backed by `~/.config/ollie/agents/`. They're plain JSON files.

```sh
ls ~/mnt/ollie/a/                                    # list agents
cat ~/mnt/ollie/a/default.json                       # read an agent config
cp ~/mnt/ollie/a/default.json ~/mnt/ollie/a/yolo.json  # copy an agent
mv ~/mnt/ollie/a/old.json ~/mnt/ollie/a/new.json     # rename
rm ~/mnt/ollie/a/scratch.json                         # delete
```

## Backends

```sh
cat ~/mnt/ollie/backends     # list available backends
```

## Help

```sh
cat ~/mnt/ollie/help         # show help (from ~/.config/ollie/help.md)
```

## Example shell session

```sh
$ echo "cwd=/home/lkn/src/ollie" > ~/mnt/ollie/s/new
$ ls ~/mnt/ollie/s/
new
1744276689123456789-2b986c
$ cd ~/mnt/ollie/s/1744276689123456789-2b986c
$ tail -f chat &
$ echo "list the go files in /home/lkn/src/ollie" > prompt
user: list the go files in /home/lkn/src/ollie
assistant: -> execute_code({"code":"find /home/lkn/src/ollie -name '*.go'","language":"bash"})
= pkg/agent/core.go
pkg/agent/loop.go
...
assistant: The Go source files are: core.go, loop.go, ...
$ cat state
idle
$ mv ~/mnt/ollie/s/1744276689123456789-2b986c ~/mnt/ollie/s/ollie-demo
$ ls ~/mnt/ollie/s/
new
ollie-demo
```

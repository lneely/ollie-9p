# olliesrv

A 9P server that exposes [ollie](../ollie) agent sessions as a virtual filesystem. Mount it with `9pfuse` and interact with AI sessions using ordinary shell tools.

The goal is integration, not self-sufficiency. Rather than providing orchestration, scheduling, or workflow primitives, olliesrv exposes a stable surface — sessions as directories, conversation as files — and defers everything else to the surrounding environment. Scripting, chaining, monitoring, and automation come from composing olliesrv with tools that already exist, not from building those capabilities into the server.

## Filesystem layout

```
ollie/
  ctl                   write: "new [backend=x] [model=x] [agent=x]" | "kill <session-id>"
  p/                    dir:   prompt templates (r/w, backed by ~/.config/ollie/prompts/)
    SYSTEM_PROMPT.md    r/w:   the system prompt template
  s/                    dir:   one entry per active session, sorted by creation time
    <session-id>/
      prompt            write: submit a prompt to the agent (clears reply)
      enqueue           write: queue a prompt for later execution
      dequeue           read:  pop the next queued prompt
      chat              read:  cumulative conversation history
      reply             read:  assistant text from the most recent turn only
      state             read:  current agent state (idle, thinking, calling: <tool>)
      ctl               write: stop | interrupt | /<slash-command>
      backend           r/w:   active backend name
      agent             r/w:   active agent name
      model             r/w:   active model name
      workdir           r/w:   working directory for tool execution and system prompt
  sk/                   dir:   skills (r/o, from OLLIE_SKILLS_PATH or ~/.config/ollie/skills/)
    <name>.md           read:  skill SKILL.md content
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
echo "new workdir=/home/lkn/src/myproject" > ~/mnt/ollie/ctl
echo "new workdir=/home/lkn/src/myproject backend=ollama" > ~/mnt/ollie/ctl
echo "new workdir=/home/lkn/src/myproject backend=ollama model=qwen3:8b" > ~/mnt/ollie/ctl
echo "new workdir=/home/lkn/src/myproject backend=ollama model=qwen3:8b agent=myagent" > ~/mnt/ollie/ctl
```

All options are optional and can be specified in any order. Unrecognised keys are rejected.
Valid keys: `backend`, `model`, `agent`, `workdir`.

A new session directory appears under `s/`, named by Unix nanosecond timestamp + random suffix (e.g. `1744276689123456789-2b986c`).

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

The `chat` file is an append-only log of the full conversation. Format:

```
user: <prompt>
assistant: <response>
-> <tool>(<args>)
= <result>
```

### Check agent state

```sh
cat ~/mnt/ollie/s/<session-id>/state
# idle | thinking | calling: <toolname>
```

### Control a session

```sh
echo stop > ~/mnt/ollie/s/<session-id>/ctl          # interrupt the current turn
echo /compact > ~/mnt/ollie/s/<session-id>/ctl      # summarize context
echo /clear > ~/mnt/ollie/s/<session-id>/ctl        # clear session history
echo /model qwen3:8b > ~/mnt/ollie/s/<session-id>/ctl
```

`ctl` accepts `stop`/`interrupt` or any `/slash-command` supported by the agent. Arbitrary text is rejected.

### Switch backend, model, or agent

```sh
echo ollama > ~/mnt/ollie/s/<session-id>/backend
echo qwen3:8b > ~/mnt/ollie/s/<session-id>/model
echo myagent > ~/mnt/ollie/s/<session-id>/agent
```

### Kill a session

```sh
echo "kill <session-id>" > ~/mnt/ollie/ctl
```

## Example shell session

```sh
$ echo "new workdir=/home/lkn/src/ollie" > ~/mnt/ollie/ctl
$ ls ~/mnt/ollie/s/
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
```


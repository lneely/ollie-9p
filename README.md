# olliesrv

A 9P server that exposes [ollie](../ollie) agent sessions as a virtual filesystem. Mount it with `9pfuse` and interact with AI sessions using ordinary shell tools.

## Filesystem layout

```
ollie/
  ctl                   write: "new [backend=x] [model=x] [agent=x]" | "kill <session-id>"
  <session-id>/
    prompt              write: submit a prompt to the agent
    chat                read:  cumulative conversation history
    state               read:  current agent state (idle, thinking, calling: <tool>)
    ctl                 write: stop | interrupt | /<slash-command>
    backend             r/w:   active backend name
    agent               r/w:   active agent name
    model               r/w:   active model name
```

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
echo new > ~/mnt/ollie/ctl                                        # all defaults
echo "new backend=ollama" > ~/mnt/ollie/ctl                       # specific backend
echo "new backend=ollama model=qwen3:8b" > ~/mnt/ollie/ctl        # backend + model
echo "new backend=ollama model=qwen3:8b agent=myagent" > ~/mnt/ollie/ctl
```

All options are optional and can be specified in any order. Unrecognised keys are rejected.
Valid keys: `backend`, `model`, `agent`.

A new session directory appears under the mount point named by timestamp + random suffix (e.g. `20260410-014002-ba70fc`).

### Send a prompt

```sh
echo "what files are in the current directory?" > ~/mnt/ollie/<session-id>/prompt
```

Writes dispatch asynchronously on close, so the shell returns immediately. The agent runs in the background.

### Read the conversation

```sh
cat ~/mnt/ollie/<session-id>/chat             # full history snapshot
tail -f ~/mnt/ollie/<session-id>/chat         # follow output as it arrives
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
cat ~/mnt/ollie/<session-id>/state
# idle | thinking | calling: <toolname>
```

### Control a session

```sh
echo stop > ~/mnt/ollie/<session-id>/ctl          # interrupt the current turn
echo /compact > ~/mnt/ollie/<session-id>/ctl      # summarize context
echo /clear > ~/mnt/ollie/<session-id>/ctl        # clear session history
echo /model qwen3:8b > ~/mnt/ollie/<session-id>/ctl
```

`ctl` accepts `stop`/`interrupt` or any `/slash-command` supported by the agent. Arbitrary text is rejected.

### Switch backend, model, or agent

```sh
echo ollama > ~/mnt/ollie/<session-id>/backend
echo qwen3:8b > ~/mnt/ollie/<session-id>/model
echo myagent > ~/mnt/ollie/<session-id>/agent
```

### Kill a session

```sh
echo "kill <session-id>" > ~/mnt/ollie/ctl
```

## Possible Applications

**Scripting and automation** — shell scripts that submit prompts, poll `state` until idle, then read `chat` for the result. No SDK, no HTTP client, just file I/O. Works in any language that can write to a file.

**Multiplexing sessions** — run several agents in parallel, each in their own session directory, and fan work out to them from a shell script. Coordinate by watching their `state` files.

**Integration with existing Unix tools** — pipe `chat` into `grep`, `awk`, `sed`. Diff two sessions' outputs. Log conversations with `cp`. Search history across sessions with `grep -r`.

**Editor integration** — any editor that can read/write files gets agent access for free. In `acme` or `sam`, you write to `prompt` and read back `chat` with no plugin required. Fits naturally into the Plan 9 workflow.

**Lightweight TUI alternatives** — `watch cat state` as a status bar, `tail -f chat` in one pane, prompt submission in another. Compose a working interface entirely from standard tools.

**Remote access** — 9P is a network protocol. Export the namespace over the network and access agent sessions from another machine using the same file interface, with no additional daemon or API layer.

**Agent chaining** — one agent's `chat` output piped into another session's `prompt`. Model composition at the shell level.

**Hooks and watchers** — poll `state` in a loop to trigger actions when the agent finishes, or use `tail -f chat` to react to new output. Build lightweight event-driven pipelines with standard shell tools.

## Example shell session

```sh
$ echo new > ~/mnt/ollie/ctl
$ ls ~/mnt/ollie/
20260410-014002-ba70fc  ctl
$ sid=20260410-014002-ba70fc
$ tail -f ~/mnt/ollie/$sid/chat &
$ echo "list the go files in /home/lkn/src/ollie" > ~/mnt/ollie/$sid/prompt
user: list the go files in /home/lkn/src/ollie
assistant: -> execute_code({"code":"find /home/lkn/src/ollie -name '*.go'","language":"bash"})
= pkg/agent/core.go
pkg/agent/loop.go
...
assistant: The Go source files are: core.go, loop.go, ...
$ cat ~/mnt/ollie/$sid/state
idle
```

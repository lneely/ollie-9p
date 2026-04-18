# olliesrv

A 9P server that exposes [ollie](https://github.com/lneely/ollie) agent sessions as a virtual filesystem. Mount it with `9pfuse` and interact with AI sessions using ordinary shell tools.

The goal is integration, not self-sufficiency. Rather than providing orchestration, scheduling, or workflow primitives, olliesrv exposes a stable surface — sessions as directories, conversation as files — and defers everything else to the surrounding environment. Scripting, chaining, monitoring, and automation come from composing olliesrv with tools that already exist, not from building those capabilities into the server.

## Filesystem layout

```
ollie/
  a/                    dir:   agent configs (r/w, backed by ~/.config/ollie/agents/)
    <name>.json         r/w:   agent config; supports mv, cp, rm
  backends              read:  list of ollie-provided backends
  help                  read:  help file (backed by ~/.config/ollie/help.md)
  p/                    dir:   prompt templates (read, backed by ~/.config/ollie/prompts/)
    00_base.md          read:  tone, accuracy, task execution, environment
    01_ollie.md         read:  session identity, filesystem layout, session lifecycle
    02_reasoning.md     read:  reasoning instructions
    03_memory.md        read:  memory instructions
    04_task.md          read:  task planning instructions
    05_edit-text.md     read:  text editing instructions
    06_skills.md        read:  skill discovery and loading
    07_tools.md         read:  tool script usage

  Prompt files are assembled in lexical order and rendered as Go templates at session
  start. Changes take effect the next time the system prompt is rebuilt: on new session
  creation, `/cwd` change, or `/agent` switch.

  b/                    dir:   batch jobs and job scripts
    new                 r/w:   read: KV template; write: submit a job spec
    idx                 read:  job index (id, status, cwd, agent — one per line)
    job                 exec:  core batch job runner (submit, wait, print result)
    q                   exec:  foreground one-shot query; thin wrapper around job
    sched               exec:  submit background job; prints b/ path; wrapper around job
    <job-id>/                  rm -r to cancel and remove
      spec              read:  original job spec as submitted
      status            read:  running | done | failed: <reason>
      result            read:  assistant reply (populated when done)
      usage             read:  token counts
      ctxsz             read:  context size
  pl/                   dir:   plans
  s/                    dir:   sessions and session management scripts
    new                 r/w:   read: KV template; write: create session
    idx                 read:  session index (id, state, cwd, backend, model — one per line)
    sh                  exec:  interactive chat shell
    ls                  exec:  list active sessions
    kill                exec:  kill a session by ID
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
      state             read:  current agent state (idle, thinking, calling: <tool>)
      systemprompt      read:  fully rendered system prompt for this session
      usage             read:  token counts (input, output, requests; [estimated] if not reported by backend)
      cwd               r/w:   working directory for tool execution and system prompt
  sk/                   dir:   skills (r/w, from OLLIE_SKILLS_PATH or ~/.config/ollie/skills/)
    <name>.md           r/w:   skill SKILL.md content
  t/                    dir:   tool scripts (r/w, backed by ~/.config/ollie/tools/)
    <script>            r/w:   tool script content
  u/                    dir:   utility scripts (read, backed by ~/.config/ollie/scripts/u/)
    <script>            exec:  utility script; compositions of b/ primitives
```

Session IDs are Unix nanosecond timestamps with a random suffix (e.g. `1744276689123456789-2b986c`), so `ls s/` sorted lexicographically gives creation order.

## Backing stores

Each 9P directory endpoint is backed by a named store implementing one of the store interfaces (`ReadableStore`, `ReadWriteStore`, `BlobStore`, or `Store`). The server routes all filesystem operations through the store — it has no direct knowledge of what underlies it.

| Path  | Default backing        | Interface       |
|-------|------------------------|-----------------|
| `/a`  | `FlatDirStore`         | `Store`         |
| `/b`  | `BatchStore`           | `Store`         |
| `/m`  | `FlatDirStore`         | `Store`         |
| `/p`  | `FlatDirStore`         | `ReadableStore` |
| `/pl` | `FlatDirStore`         | `Store`         |
| `/sk` | `SkillStore`           | `Store`         |
| `/t`  | `ToolStore`            | `Store`         |
| `/u`  | `UtilStore`            | `Store`         |
| `/s`  | `SessionStore`         | `Store`         |

To swap a backing store (e.g. replace `/pl` with a vector database or `/m` with an object store), implement the appropriate interface and wire it in `New()`. The store interface requires only `Stat`, `List`, `Get`, `Put`, `Delete`, `Create`, and `Rename` — authentication, connection management, and credential rotation are internal concerns of the implementation.

`SessionStore` is the exception: it holds live session state and is tightly coupled to the server's session lifecycle. Replacing it is possible in principle but requires understanding the session management internals.

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

The server listens on a Unix socket in the Plan 9 namespace (`$NAMESPACE/ollie`) and optionally mounts via `9pfuse` to `$HOME/mnt/ollie` (or `$OLLIE`).

## Sessions

### Interactive shell with s/sh

`s/sh` is an interactive shell for prompting a session. It resumes the most recent session for the current working directory by default, or creates a new one if none exists.

```sh
exec $OLLIE/s/sh                               # resume or create session for pwd
exec $OLLIE/s/sh -new                          # always start a fresh session
exec $OLLIE/s/sh -new -model qwen3:8b          # fresh session with model override
exec $OLLIE/s/sh -session 1744276689123456789-2b986c  # attach to a specific session
```

At the `> ` prompt:

| Input | Action |
|---|---|
| `<text>` | Submit prompt; response streams to terminal |
| `/q <text>` | Enqueue prompt for later execution |
| `/<cmd>` | Send to session ctl (e.g. `/compact`, `/clear`, `/model qwen3:8b`) |
| `/kill` | Kill the session and exit |
| Ctrl-C | Interrupt a running turn (or exit if idle) |

`s/sh` tracks the last session per working directory in `~/.config/ollie/last-session`, so switching directories and re-running `s/sh` resumes the right context automatically.

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

## Batch jobs

`b/` provides ephemeral one-shot agent runs. Each job is a single prompt → single result with no persistent session state. Jobs run concurrently; each gets its own entry under `b/`.

### Submit a job

```sh
cat ~/mnt/ollie/b/new                              # show the spec template
```

Write a spec to `b/new`:

```
name=my-job
cwd=/home/lkn/src/myproject
agent=default
backend=anthropic
model=claude-sonnet-4-6
parallel=1
---
Summarize the top-level Go files in the current directory.
```

```sh
printf 'name=my-job\ncwd=%s\n---\nSummarize this repo.\n' "$PWD" > ~/mnt/ollie/b/new
```

Valid header keys: `name` (optional; auto-generated if omitted), `cwd` (required), `agent`, `backend`, `model`, `output`, `parallel`.

Setting `parallel=N` creates N independent jobs named `{name}-0` through `{name}-N-1`, all running the same prompt concurrently.

### Poll and read results

```sh
cat ~/mnt/ollie/b/<job-id>/status      # running | done | failed: <reason>
cat ~/mnt/ollie/b/<job-id>/result      # assistant reply (when done)
cat ~/mnt/ollie/b/<job-id>/usage       # token counts
cat ~/mnt/ollie/b/<job-id>/ctxsz       # context size
cat ~/mnt/ollie/b/<job-id>/spec        # original spec
cat ~/mnt/ollie/b/idx                  # all jobs: id status cwd agent
```

### Remove a job

```sh
rm -r ~/mnt/ollie/b/<job-id>           # cancel if running, then remove
```

### Shell wrappers

`b/job`, `b/q`, and `b/sched` are shell scripts that wrap the `b/` namespace for one-liners. They are exposed directly via the mounted filesystem:

```sh
$OLLIE/b/job "Summarize this repo"           # submit, wait, print result
echo "what is 2+2?" | $OLLIE/b/q            # foreground query via stdin
$OLLIE/b/sched "Run a background task"       # submit and return b/ path
$OLLIE/b/job -parallel 4 "Write a haiku"    # run N times concurrently
$OLLIE/b/job -backend ollama -model qwen3:8b "Explain this" < main.go
```

`b/q` is a thin wrapper around `b/job`. `b/sched` wraps `b/job -bg` and prints the `b/{id}` path for each submitted job.

### Background jobs with b/sched

`b/sched` submits a job and returns immediately, printing the `b/{id}` path for each job. Use it when you want to fire off work and check results later.

```sh
$OLLIE/b/sched "summarize the recent git log" > /tmp/job-path
cat /tmp/job-path
# /home/lkn/mnt/ollie/b/1744276689123456789-0
```

Poll for completion and read the result:

```sh
path=$($OLLIE/b/sched "write a limerick about Go")
until [ "$(cat $path/status)" = "done" ]; do sleep 0.1; done
cat $path/result
```

Submit multiple jobs in parallel with `-parallel N` — each gets its own path:

```sh
$OLLIE/b/sched -parallel 3 "generate a test case for this function" < main.go
# /home/lkn/mnt/ollie/b/1744276689123456789-0
# /home/lkn/mnt/ollie/b/1744276689123456789-1
# /home/lkn/mnt/ollie/b/1744276689123456789-2
```

Fan out work and collect results when all are done:

```sh
paths=$($OLLIE/b/sched -parallel 4 "draft a blog intro" < brief.txt)
for p in $paths; do
    until [ "$(cat $p/status 2>/dev/null)" = "done" ]; do sleep 0.1; done
    cat $p/result
    rm -r $p
done
```

### AI pipelines

Because `b/job` and `b/q` write results to stdout, they compose naturally with Unix pipes. Any tool that reads stdin and writes stdout is a pipeline stage.

```sh
echo "write a haiku about filesystems" | $OLLIE/b/q | wc -w
cat error.log | $OLLIE/b/q "what is causing this error?" | $OLLIE/b/q "suggest a fix"
```

Multi-stage pipelines can chain LLM calls, shell transforms, and other tools:

```sh
$OLLIE/b/q "list 5 blog post ideas" | grep -v "^$" | head -3 | $OLLIE/b/q "expand the best one"
```

`u/optimize` is a worked example of a pipeline built on `b/q`. It generates N candidate prompts in parallel, then judges them to return the best:

```sh
$OLLIE/u/optimize -n 3 -cm qwen/qwen3-8b -jm anthropic/claude-opus-4-6 "explain recursion"
```

Internally, `u/optimize` calls `b/q -parallel N` for candidate generation and `b/q` again for judging — two LLM stages composed via shell variables, with stderr used for progress and stdout carrying the result. The output can be piped directly into another stage:

```sh
$OLLIE/u/optimize "translate this to Spanish" >[2]/dev/null | $OLLIE/b/q < input.txt
```

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

# olliesrv

A 9P server that exposes [ollie](https://github.com/lneely/ollie) agent sessions as a virtual filesystem. Mount it with `9pfuse` and interact with AI sessions using ordinary shell tools.

The goal is integration, not self-sufficiency. Rather than providing orchestration, scheduling, or workflow primitives, olliesrv exposes a stable surface — sessions as directories, conversation as files — and defers everything else to the surrounding environment. Scripting, chaining, monitoring, and automation come from composing olliesrv with tools that already exist, not from building those capabilities into the server.

For usage examples, see [doc/USAGE.md](https://github.com/lneely/ollie/blob/main/doc/USAGE.md) in the monorepo.

## Filesystem layout

```
ollie/
  a/                         dir:   agent configs (r/w, backed by ~/.config/ollie/agents/)
    <name>.json              r/w:   agent config; supports mv, cp, rm
  backends                   read:  list of ollie-provided backends
  help                       read:  help file (backed by ~/.config/ollie/help.md)
  p/                         dir:   prompt templates (read, backed by ~/.config/ollie/prompts/)
    00_base.md               read:  tone, accuracy, task execution, environment
    01_ollie.md              read:  session identity, filesystem layout, session lifecycle
    02_skills.md             read:  skill discovery and loading
    03_tool-files.md         read:  text editing instructions
    04_tool-reasoning.md     read:  reasoning instructions
    05_tool-memory.md        read:  memory instructions
    07_tool-subagents.md     read:  subagent usage
    08_tool-elevate.md       read:  controlled escape from execute_code sandbox

  Prompt files have no automatic effect. They are injected into the system prompt
  by the `prime` script (x/prime), typically via agentSpawn hooks in agent configs.
  The assembled system prompt is visible at s/<id>/systemprompt.

  b/                    dir:   batch jobs and job scripts
    new                 r/w:   read: KV template; write: submit a job spec
    idx                 read:  job index (id, status, cwd, agent — one per line)
    job                 exec:  core batch job runner (submit, wait, print result)
    q                   exec:  foreground one-shot query; thin wrapper around job
    sched               exec:  submit background job; prints b/ path; wrapper around job
    cleanup             exec:  remove all jobs with status "done"
    <job-id>/                  rm -r to cancel and remove
      spec              read:  original job spec as submitted
      status            read:  running | done | failed: <reason>
      result            read:  assistant reply (populated when done)
      usage             read:  token counts
      ctxsz             read:  context size
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
  x/                    dir:   system exec and plugins (read, backed by ~/.config/ollie/scripts/x/)
    <plugin>            exec:  server-invoked plugin (e.g. elevation backends)
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
| `/sk` | `SkillStore`           | `Store`         |
| `/t`  | `ToolStore`            | `Store`         |
| `/u`  | `UtilStore`            | `Store`         |
| `/x`  | `ExecStore`            | `Store`         |
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

### Create

```sh
cat $OLLIE/s/new                    # show required/optional KV pairs
echo "cwd=$PWD" > $OLLIE/s/new
echo "cwd=$PWD backend=ollama model=qwen3:8b" > $OLLIE/s/new
```

Valid keys: `cwd` (required), `backend`, `model`, `agent`.

### Send a prompt

```sh
echo "what files are in the current directory?" > $OLLIE/s/<session-id>/prompt
```

Writes dispatch asynchronously on close; the shell returns immediately.

### Read the conversation

```sh
cat $OLLIE/s/<session-id>/chat          # full history snapshot
tail -f $OLLIE/s/<session-id>/chat      # follow output as it arrives
```

### Check state

```sh
cat $OLLIE/s/<session-id>/state
# idle | thinking | calling: <toolname>
```

### Control

```sh
echo stop    > $OLLIE/s/<session-id>/ctl   # interrupt current turn
echo compact > $OLLIE/s/<session-id>/ctl   # summarize context
echo clear   > $OLLIE/s/<session-id>/ctl   # clear history
echo kill    > $OLLIE/s/<session-id>/ctl   # kill session
echo "rn my-name" > $OLLIE/s/<session-id>/ctl
echo "model qwen3:8b" > $OLLIE/s/<session-id>/ctl
```

`ctl` accepts only recognized commands: `stop`, `kill`, `rn <name>`, `compact`, `clear`, `backend`, `model`, `models`, `agents`, `agent`, `sessions`, `cwd`, `skills`, `tools`, `mcp`, `context`, `usage`, `history`, `irw`, `help`. The `/` prefix is added automatically. Unrecognized input is rejected with an error.

### Switch backend, model, or agent

```sh
echo ollama   > $OLLIE/s/<session-id>/backend
echo qwen3:8b > $OLLIE/s/<session-id>/model
echo myagent  > $OLLIE/s/<session-id>/agent
```

Writes to `backend`, `model`, and `agent` are rejected when the agent is not idle. Check `state` to confirm the change took effect.

### Kill and rename

```sh
rm -r $OLLIE/s/<session-id>                          # kill
mv $OLLIE/s/<session-id> $OLLIE/s/my-friendly-name  # rename
```

Rename is rejected if the agent is running or the target name already exists. All open file handles into the session are updated automatically.

## Agents

Agent configs live in `a/` and are backed by `~/.config/ollie/agents/`. They're plain JSON files.

```sh
ls $OLLIE/a/                                  # list agents
cat $OLLIE/a/default.json                     # read config
cp $OLLIE/a/default.json $OLLIE/a/yolo.json  # copy
mv $OLLIE/a/old.json $OLLIE/a/new.json       # rename
rm $OLLIE/a/scratch.json                      # delete
```

## Example shell session

```sh
$ echo "cwd=$PWD" > $OLLIE/s/new
$ ls $OLLIE/s/
new
1744276689123456789-2b986c
$ cd $OLLIE/s/1744276689123456789-2b986c
$ tail -f chat &
$ echo "list the go files in $PWD" > prompt
user: list the go files in /home/lkn/src/ollie
assistant: -> execute_code({"code":"find . -name '*.go'","language":"bash"})
= pkg/agent/core.go
  pkg/agent/loop.go
  ...
assistant: The Go source files are: core.go, loop.go, ...
$ cat state
idle
$ mv $OLLIE/s/1744276689123456789-2b986c $OLLIE/s/ollie-demo
```

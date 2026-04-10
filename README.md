# olliesrv

A 9P server that exposes [ollie](../ollie) agent sessions as a virtual filesystem. Mount it with `9pfuse` and interact with AI sessions using ordinary shell tools.

The goal is integration, not self-sufficiency. Rather than providing orchestration, scheduling, or workflow primitives, olliesrv exposes a stable surface — sessions as directories, conversation as files — and defers everything else to the surrounding environment. Scripting, chaining, monitoring, and automation come from composing olliesrv with tools that already exist, not from building those capabilities into the server.

## Filesystem layout

```
ollie/
  ctl                   write: "new [backend=x] [model=x] [agent=x]" | "kill <session-id>"
  s/                    dir:   one entry per active session, sorted by creation time
    <session-id>/
      prompt            write: submit a prompt to the agent (clears reply)
      chat              read:  cumulative conversation history
      reply             read:  assistant text from the most recent turn only
      state             read:  current agent state (idle, thinking, calling: <tool>)
      ctl               write: stop | interrupt | /<slash-command>
      backend           r/w:   active backend name
      agent             r/w:   active agent name
      model             r/w:   active model name
      workdir           r/w:   working directory for tool execution and system prompt
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
echo new > ~/mnt/ollie/ctl                                        # all defaults
echo "new backend=ollama" > ~/mnt/ollie/ctl                       # specific backend
echo "new backend=ollama model=qwen3:8b" > ~/mnt/ollie/ctl        # backend + model
echo "new backend=ollama model=qwen3:8b agent=myagent" > ~/mnt/ollie/ctl
echo "new workdir=/home/lkn/src/myproject" > ~/mnt/ollie/ctl             # set working directory
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

## Ideas

### Automation & Scripting

**Scripting and automation** — shell scripts that submit prompts, poll `state` until idle, then read `chat` for the result. No SDK, no HTTP client, just file I/O. Works in any language that can write to a file.

**Multiplexing sessions** — run several agents in parallel, each in their own session directory, and fan work out to them from a shell script. Coordinate by watching their `state` files.

**Hooks and watchers** — poll `state` in a loop to trigger actions when the agent finishes, or use `tail -f chat` to react to new output. Build lightweight event-driven pipelines with standard shell tools.

### Tooling & Integration

**Unix tools** — pipe `chat` into `grep`, `awk`, `sed`. Diff two sessions' outputs. Log conversations with `cp`. Search history across sessions with `grep -r`.

**Editor integration** — any editor that can read/write files gets agent access for free. In `acme` or `sam`, you write to `prompt` and read back `chat` with no plugin required. Fits naturally into the Plan 9 workflow.

**Lightweight TUI alternatives** — `watch cat state` as a status bar, `tail -f chat` in one pane, prompt submission in another. Compose a working interface entirely from standard tools.

### Network & Remote Access

**Remote access** — 9P is a network protocol. Export the namespace over the network and access agent sessions from another machine using the same file interface, with no additional daemon or API layer.

### Multi-Agent Workflows

Each session exposes a `reply` file containing only the assistant text from the most recently completed turn. Writing to `prompt` clears `reply` as a side-effect. Together these make agent-to-agent handoffs expressible as plain file operations — no message queue, no orchestration framework, no shared memory.

A shell script moves text between sessions. The agents communicate only through what the script explicitly passes between them, so information flow is fully under your control. Each agent can use a different backend, model, or tool configuration. A human can intervene at any point by writing directly to a session's `prompt`.

#### Example: develop → review → test

Three sessions with specialized agent configs run a feedback loop. The reviewer ends its response with `LGTM` (proceed) or `PTAL` (revise). The tester ends with `Approved` (done) or `Rejected` (revise). A shell script reads the verdict with `grep` and routes accordingly.

```sh
#!/bin/sh
cd ~/mnt/ollie

# Count existing sessions so we can identify the newly created ones by offset.
n=$(ls s/ | wc -l)

echo "new agent=developer" > ctl
echo "new agent=reviewer"  > ctl
echo "new agent=tester"    > ctl

dev=$(ls s/ | sort | sed -n "$((n+1))p")
rev=$(ls s/ | sort | sed -n "$((n+2))p")
tst=$(ls s/ | sort | sed -n "$((n+3))p")

wait_reply() {
    while [ "$(wc -c < s/$1/reply)" -eq 0 ]; do sleep 1; done
}

echo "implement a function that parses a JSON config file" > s/$dev/prompt

while true; do
    wait_reply $dev
    code=$(cat s/$dev/reply)

    # review phase
    printf "Review the following code. End your response with LGTM if it is ready, or PTAL if it needs revision.\n\n%s" "$code" > s/$rev/prompt
    wait_reply $rev

    if ! grep -qi "LGTM" s/$rev/reply; then
        { echo "revise this code based on the feedback below."
          echo "--- code ---";     echo "$code"
          echo "--- feedback ---"; cat s/$rev/reply
        } > s/$dev/prompt
        continue
    fi

    # test phase
    printf "Test the following code. End your response with Approved if all tests pass, or Rejected if they do not.\n\n%s" "$code" > s/$tst/prompt
    wait_reply $tst

    if grep -qi "Approved" s/$tst/reply; then
        echo "done."
        echo "$code"
        break
    fi

    { echo "fix the failures reported below."
      echo "--- code ---";        echo "$code"
      echo "--- test report ---"; cat s/$tst/reply
    } > s/$dev/prompt
done
```

Each agent only sees what the script explicitly sends it. Each can use a different backend, model, or tool configuration. A human can intervene at any point by writing directly to a session's `prompt`. The boundary between fully automated and human-in-the-loop is just whether the script pauses to ask.

### Sub-Agents

A session can spawn ephemeral child sessions to handle a focused task, then tear them down when done. The parent uses `execute_code` to write `new` to `ctl`, send an initial context and task to the child's `prompt`, poll until `reply` is ready, read the result, and write `kill <id>` to `ctl`. From the child's perspective it is just a normal session; it has no knowledge of its own ephemerality.

**Sub-agents as function calls** — the parent primes the child with exactly the context it needs (excerpts from its own `chat`, relevant beads, local file contents) and receives a single focused reply. Failures are isolated; a child that errors or stalls can be killed and retried without affecting the parent. The pattern composes naturally: a sub-agent can itself spawn sub-agents, building a call tree bounded only by the number of sessions open at once.

`spawn_subagents.py` reads a JSON spec from stdin, fans out the work concurrently, and returns a JSON array of results — a single `execute_tool` call from the parent's perspective. Note: this requires adding Python language support to `execute_code`, `execute_tool`, and `execute_pipe`, which is low effort.

```python
#!/usr/bin/env python3
"""spawn_subagents.py — fan out tasks to ephemeral olliesrv sub-agents.

Input  (stdin):  JSON array of {"task": str, "agent": str, "context": str}
Output (stdout): JSON array of {"agent": str, "reply": str}

Install in $OLLIE_TOOLS_PATH to make available via execute_tool.
"""

import json, os, sys, threading, time
from concurrent.futures import ThreadPoolExecutor, as_completed
from pathlib import Path

BASE         = Path.home() / "mnt" / "ollie"
POLL         = 0.5
_spawn_lock  = threading.Lock()


def session_ids():
    return {e.name for e in (BASE / "s").iterdir() if e.is_dir()}

def spawn_session(agent="default"):
    with _spawn_lock:
        before = session_ids()
        (BASE / "ctl").write_text(f"new agent={agent}\n")
        while True:
            new = session_ids() - before
            if new:
                return new.pop()
            time.sleep(0.1)

def kill_session(sid):
    (BASE / "ctl").write_text(f"kill {sid}\n")

def wait_reply(sid):
    path = BASE / "s" / sid / "reply"
    while True:
        if path.stat().st_size > 0:
            return path.read_text().strip()
        time.sleep(POLL)

def run_subagent(spec):
    agent   = spec.get("agent", "default")
    context = spec.get("context", "")
    task    = spec.get("task", "")
    sid     = spawn_session(agent)
    try:
        prompt = f"{context}\n\n{task}".strip() if context else task
        (BASE / "s" / sid / "prompt").write_text(prompt)
        return {"agent": agent, "reply": wait_reply(sid)}
    finally:
        kill_session(sid)

def main():
    specs   = json.load(sys.stdin)
    results = [None] * len(specs)
    with ThreadPoolExecutor(max_workers=len(specs)) as pool:
        futures = {pool.submit(run_subagent, s): i for i, s in enumerate(specs)}
        for f in as_completed(futures):
            results[futures[f]] = f.result()
    json.dump(results, sys.stdout, indent=2)
    print()

if __name__ == "__main__":
    main()
```

The parent agent constructs the spec and makes a single tool call:

```json
[
  { "task": "review this code for correctness", "agent": "reviewer", "context": "..." },
  { "task": "check this code for security issues", "agent": "security", "context": "..." }
]
```

Results come back as:

```json
[
  { "agent": "reviewer", "reply": "..." },
  { "agent": "security", "reply": "..." }
]
```

### Self-Generating Workflows

Since agents have access to `execute_code`, a session can write and execute a workflow script without any human involvement. Given a task and knowledge of the filesystem layout, an agent can decompose the work, spawn sessions, write the coordination script, and run it — all in a single turn. The README you are reading is essentially its system prompt. Conductor functionality, for free.

### Persistent Memory

**Task tracking** — Agent sessions are ephemeral by nature: context windows fill, processes crash, work gets lost. [9beads](https://github.com/lneely/9beads) addresses this by exposing a structured, version-controlled task database as a 9P filesystem. Any session can read the task list, claim work, track dependencies, and mark completion through ordinary file operations. If a session is interrupted, another can pick up exactly where it left off. Task lists are scoped per-project, keeping them focused and bounded. Combined with olliesrv, agents get both a communication surface and a memory layer, with no special integration required between them.

## Example shell session

```sh
$ echo new > ~/mnt/ollie/ctl
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

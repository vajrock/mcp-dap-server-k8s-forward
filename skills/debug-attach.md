---
name: debug-attach
description: |
  Live debugging by attaching to a running process (local by PID) OR to a pre-connected remote dlv in a Kubernetes pod via this fork's ConnectBackend setup.
  TRIGGER when: user asks to debug a running process, diagnose a live process by PID, attach to an already-running program, investigate live CPU/memory/deadlock issues, or debug a Go service running in a Kubernetes pod that already has dlv --headless listening.
  DO NOT TRIGGER when: debugging from source (use debug-source), analyzing a crash dump (use debug-core-dump), or the process hasn't started yet (use debug-source or debug-binary).
---

# Live Attach Debug Workflow

## Two sub-scenarios — identify which you're in BEFORE calling debug

### Sub-scenario (a): LOCAL attach by PID

You want to attach to a process running **on the same machine** where this
mcp-dap-server is running. You need the PID, `ptrace_scope` permissions,
and a compatible debugger in `$PATH`.

### Sub-scenario (b): PRE-CONNECTED (remote k8s) attach

This MCP server was **pre-configured** by the operator's wrapper script
(typically `dlv-k8s-mcp.sh`) with a `--connect <localhost:PORT>` flag or
`DAP_CONNECT_ADDR` env var. A `kubectl port-forward` is already running in
the wrapper's supervisor loop, and `dlv --headless --accept-multiclient`
is listening inside a k8s pod.

**You do NOT:** manage port-forward, discover a PID, pass a port, pass a
connect address, or worry about ConnectBackend mechanics. All of that is
operator setup in `.mcp.json`.

**How to tell which sub-scenario you're in:**

- If the user says anything like "debug the Go service in dev" / "the
  kov-dev server" / "in the pod" / "the service running in k8s" — it is
  almost certainly (b).
- If the user says "my running process with PID 12345" / "this local
  daemon" — it is (a).
- If unsure, try `debug(mode="attach")` with NO processId. If the server
  is in (b) it will work; if in (a) you'll get a clear error
  (`processId is required for attach mode`) and you can re-call with it.

## Pre-flight checklist

Before starting, gather:
1. **Sub-scenario (a) or (b)?** See above.
2. **(a) only:** **PID** of the target process (`pgrep <name>` / `ps`).
3. **What is the observed problem?** (wrong response, hang, deadlock,
   memory leak, unexpected behavior on specific request)
4. **Trigger:** how will you cause the breakpoint to hit? HTTP request
   via chrome-devtools, curl, a specific user action? Prepare it before
   setting breakpoints — wait-for-stop will block until the trigger
   fires.
5. **Is this a production process?** Setting breakpoints pauses it for
   all users. For (b) in a shared dev/staging namespace, the same
   caveat applies at a smaller scale.

## Important warnings

- Attaching pauses the process the moment a breakpoint fires — in
  production that affects real users; in a shared dev environment, it
  affects teammates.
- `stop()` terminates the debuggee. For attach you almost always want
  `stop(detach=true)` to leave the process running after your session.
- For sub-scenario (a): you may need `sudo` or `ptrace_scope=0`.
- For sub-scenario (b): **never** call `stop()` during an active session
  that other developers may be using. Prefer `stop(detach=true)` or just
  let the session close naturally.

---

## Step-by-Step Workflow

### 1. Attach to the process

**Sub-scenario (a) — LOCAL:**
```json
debug(mode="attach", processId=<PID>)
```

**Sub-scenario (b) — PRE-CONNECTED (k8s):**
```json
debug(mode="attach")
```

No `processId`, no port, no address. The pre-configured ConnectBackend
routes through the wrapper's port-forward to the remote dlv.

Expected result in **both** sub-scenarios (v0.2.1+): the call returns
**immediately** with a readiness message. The debuggee is already
running; no stop event will arrive without a trigger.

If attach fails:
- Sub-scenario (a): verify PID, check `ptrace_scope`, try sudo.
- Sub-scenario (b): tail `/tmp/mcp-dap-server.<ns>-<svc>.latest.log` — if
  no "ConnectBackend mode" startup banner, the server wasn't
  pre-configured and you're accidentally in (a). Ask the operator.

### 2. Understand what the process was doing

The `debug()` call already returns the initial context at the moment of attach — review it immediately. Key questions:
- **Where is it?** What function and file?
- **Why is it there?** Does the stack trace make sense?
- **What are the local values?** Do they look reasonable?

If the process was in a system call (I/O, sleep, mutex wait), the stack will show that explicitly.

Call `context()` again after resuming (e.g., after `continue()` or `pause()`) to refresh the current state.

### 3. Check all threads / goroutines

```json
info(kind="threads")
```

This is critical for concurrent programs. Look for:
- Threads blocked on the **same lock or channel** → potential deadlock
- **More threads than expected** → goroutine/thread leak
- Threads in **unexpected functions** → processing wrong data or stuck in error path

For each suspicious thread:
```json
context(threadId=<ID>)
```

**Go-specific indicators:**
- `sync.(*Mutex).Lock` or `<-chan` in every goroutine's stack → classic deadlock
- Many goroutines in `runtime.park` → goroutines blocked on channel/select

**C/C++-specific indicators:**
- `pthread_mutex_lock` or `futex` in every thread's stack → mutex deadlock
- Thread in `__GI___poll` or `epoll_wait` → waiting on I/O (usually expected)
- Thread in `malloc` / `free` with another in `malloc` → heap lock contention

### 4. Scenario-specific investigation

#### High CPU usage

Pause the process several times and look for patterns:
```json
pause()   // if it was resumed
context()
```

Do this 3-5 times. If the same function keeps appearing, that's the hot path.

Look for:
- Tight loops with no I/O or sleep
- Repeated work that should be cached
- Unexpected recursion or redundant computation

#### Deadlock / hang (process not progressing)

After attach, all threads should be visible. Look for:
- Thread A blocked waiting for lock X
- Thread B holding lock X and waiting for lock Y
- Thread C holding lock Y and waiting for lock X

Use `context(threadId=<ID>)` on each blocked thread to see what lock/channel it's waiting on.

**Go red flag:** `sync.(*Mutex).Lock` or `<-chan` in every goroutine's stack → classic deadlock.
**C/C++ red flag:** `pthread_mutex_lock` stacked below a function that also calls `pthread_mutex_lock` → lock ordering issue.

#### Memory growth / leak

**Go — check collection sizes:**
```json
evaluate(expression="len(cache)")
evaluate(expression="len(connections)")
evaluate(expression="cap(buffer)")
```

**C/C++ — inspect pointer chains and reference counts:**
```json
evaluate(expression="list->size")
evaluate(expression="pool->count")
evaluate(expression="obj->refcount")
```

Look for:
- Collections that grow but never shrink
- Connection/object pools that accumulate but don't release
- Goroutines / threads accumulating in `info(kind="threads")`

#### Unexpected behavior / wrong results

Set a targeted breakpoint at the function that produces wrong output:
```json
breakpoint(function="packageName.FunctionName")
continue()
wait-for-stop(timeoutSec=60, pauseIfTimeout=true)
```

`continue` returns immediately with `{"status":"running"}` since v0.2.0;
`wait-for-stop` blocks until the breakpoint is hit or until the timeout
elapses. Set a longer `timeoutSec` (up to 300) when the action that would
trigger the breakpoint depends on external events. When it hits, inspect
inputs and internal state to find where the logic diverges.

### 5. Iterate

Resume the process and let it run to your next observation point:
```json
continue()
wait-for-stop(timeoutSec=60, pauseIfTimeout=true)
```

Or manually pause again:
```json
pause()
context()
```

### 6. Conclude and detach

State findings clearly:
> **The process is stuck in** `FunctionName` **at** `file.go:42` **because** `mutex.Lock()` **is blocked waiting for a lock held by goroutine** `threadId=3`.
> **Root cause:** goroutine 3 is holding lock A while waiting for lock B; goroutine 1 holds lock B while waiting for lock A — circular deadlock.

To **terminate** the debuggee:
```json
stop()
```

To **detach** and leave the process running:
```json
stop(detach=true)
```

---

## Decision Tree

```
Process attached
    │
    ├─ Is every thread blocked? → Deadlock
    │      → Check all threads for mutual lock dependencies
    │
    ├─ Is one thread consuming CPU? → Hot path / infinite loop
    │      → Pause multiple times, look for repeating call site
    │
    ├─ Are threads growing over time? → Goroutine leak
    │      → Find goroutines that never finish
    │
    └─ Thread behavior looks normal → Behavioral bug
           → Set breakpoints at the function producing wrong output
```

## How to present findings

> **Diagnosis:** The process is experiencing a deadlock between goroutines 1 and 3.
> **Evidence:** Goroutine 1 is at `sync.Mutex.Lock` waiting for lock B (held by goroutine 3). Goroutine 3 is at `sync.Mutex.Lock` waiting for lock A (held by goroutine 1).
> **Root cause:** `ProcessRequest` acquires locks in order A→B, while `HandleCallback` acquires them B→A. This creates a lock-ordering inversion.
> **Fix:** Establish a consistent lock acquisition order across all code paths.

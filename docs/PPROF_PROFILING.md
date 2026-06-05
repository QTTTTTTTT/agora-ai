# Runtime profiling with pprof

When the server starts to misbehave in a way that metrics + logs
can't pin down, the canonical Go answer is `pprof`. This doc
covers when to enable it, how to take the four most useful
profiles, and how to interpret each.

## Enabling

`pprof` is **off by default**. Set `PPROF_ENABLED=1` on the
process (env var, compose `environment:`, or k8s secret) and
restart. The startup log will confirm:

```
INFO pprof endpoints enabled prefix=/debug/pprof/
```

The endpoints are:

```
GET /debug/pprof/                — index page (HTML)
GET /debug/pprof/heap            — heap snapshot (sampled allocs)
GET /debug/pprof/goroutine       — current goroutine stacks
GET /debug/pprof/allocs          — all allocations since process start
GET /debug/pprof/profile?seconds=N — CPU profile, samples for N seconds
GET /debug/pprof/trace?seconds=N   — exec trace, samples for N seconds
GET /debug/pprof/cmdline         — process cmdline
GET /debug/pprof/symbol          — used by go tool pprof
```

Production guidance: enable on a single debug pod (or expose on
a private admin port — see future work in `pprof.go`). Don't
enable on the whole fleet — `cpu profile` and `trace` add load
to the sampled process.

## The four profiles you'll actually use

### 1. Heap snapshot — "why is RSS climbing?"

```bash
go tool pprof http://localhost:8080/debug/pprof/heap
(pprof) top
(pprof) list <function-name>
```

`top` shows the functions with the most live memory at sample
time; `list` drops you into the source with annotated allocation
sizes. Most useful when investigating slow growth (cache that
never evicts, slice that's appended forever). Take two heap
profiles 5+ minutes apart and diff them with
`go tool pprof -base heap1 heap2` to see what's actually growing.

### 2. Goroutine dump — "are we leaking goroutines?"

```bash
curl http://localhost:8080/debug/pprof/goroutine?debug=1 > goroutines.txt
```

`debug=1` gives a human-readable stack listing instead of the
binary protobuf. Search the dump for stacks that occur thousands
of times — those are the leaked ones. Common culprits:

- a worker pool that re-spawns on retry but doesn't bound the
  total,
- channel reads that block forever because the producer was
  cancelled but the consumer wasn't notified,
- `time.Tick` in a function that returns (you must use
  `time.NewTicker` and `defer ticker.Stop()`).

### 3. CPU profile — "why is the pod hot?"

```bash
go tool pprof http://localhost:8080/debug/pprof/profile?seconds=30
(pprof) top
(pprof) web   # opens a flamegraph in your browser
```

The 30s sample window is long enough to catch periodic work
(workflow tick, reflection batch) but short enough that you
can iterate. Use `web` for the flamegraph view —
"thick at the bottom, narrow at the top" is healthy; "wide
plateaus near the top" usually means a hot leaf function is
chewing CPU and is a refactor candidate.

### 4. Exec trace — "where did this single request go?"

```bash
curl http://localhost:8080/debug/pprof/trace?seconds=10 > trace.out
go tool trace trace.out
```

The web UI from `go tool trace` shows the goroutine timeline,
syscalls, GC pauses, and channel sends. Use this when a
request is mysteriously slow but CPU and heap profiles look
fine — usually the answer is "we're blocked on a syscall" or
"GC stopped the world for 200ms during a 1GB heap copy".

## Interpreting common patterns

| Symptom | Likely diagnosis | Profile to use |
|---|---|---|
| RSS keeps growing, no leak in heap | goroutine leak (each leaked goroutine pins ~8 KB stack) | `goroutine` |
| RSS grows then plateaus | normal cache fill | `heap` (verify cache obj is the source) |
| p99 latency spikes correlated with GC | heap thrash | `allocs` to find the alloc-hot path; reduce per-request alloc |
| CPU high, p99 unchanged | wasted work (unused result, dead code) | `profile` |
| Pod hangs, no request completes | deadlock | `goroutine?debug=2` (full stacks) |

## Safety / access

- The pprof endpoints leak some internal info (function names,
  cmdline). They're gated behind `PPROF_ENABLED=1` so a
  misconfigured prod pod can't accidentally expose them.
- `/debug/pprof/profile` and `/debug/pprof/trace` add measurable
  load (~3-7% CPU during the sample window). Use during a
  controlled debug session, not in a hot incident.
- The endpoints live under `/debug/pprof/`, which is
  intentionally OUTSIDE `/api/*`. The auth middleware skips
  them, so they're reachable without a session — gate with
  `PPROF_ENABLED` + private port + (future) token auth.

## See also

- [docs/PROMETHEUS_QUERIES.md](PROMETHEUS_QUERIES.md) — what to
  look at first before reaching for pprof.
- [docs/RELEASE_QA_PLAYBOOK.md](RELEASE_QA_PLAYBOOK.md) —
  load-testing baseline (`scripts/perf-load-baseline.sh`) gives
  you the reference behaviour to compare a slow run against.

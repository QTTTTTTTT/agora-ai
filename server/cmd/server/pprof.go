// pprof.go — runtime profiling endpoints, gated by env flag.
//
// WHY THIS EXISTS
// ---------------
// When the app starts to misbehave in a way that metrics + logs
// can't pin down (a goroutine leak after a workflow burst, an
// unexpected heap growth on a long-running pod, an off-CPU
// stall during LLM streaming), the canonical Go answer is
// `pprof`. Today the binary has no pprof endpoint, so an SRE
// has to either:
//   - rebuild the binary with `import _ "net/http/pprof"` and
//     redeploy mid-incident (slow, risky), or
//   - SSH into the container and use `kill -SIGQUIT` for a
//     stack dump (works for goroutine state, useless for heap
//     / cpu profiles).
//
// This file registers the standard pprof handlers under
// `/debug/pprof/` IF AND ONLY IF the `PPROF_ENABLED=1` env var
// is set. Off by default for two reasons:
//   1. `/debug/pprof/profile?seconds=30` reveals enough about
//      runtime behaviour and routine names that we'd rather
//      production-by-default not expose it. It's an attack
//      surface (small, but real).
//   2. Some pprof endpoints (cpu profile, trace) put non-trivial
//      load on the process while sampling. Op-flag gating means
//      a tired SRE can't accidentally pin a hot pod by curling
//      pprof against the wrong host.
//
// ACCESS
// ------
// In dev: PPROF_ENABLED=1 in `.env` or compose env, then:
//   go tool pprof http://localhost:8080/debug/pprof/heap
//   go tool pprof http://localhost:8080/debug/pprof/goroutine
//   go tool pprof http://localhost:8080/debug/pprof/profile?seconds=30
//   go tool pprof http://localhost:8080/debug/pprof/allocs
//
// In prod: the production deploy should set PPROF_ENABLED=1
// only on a debug pod (or expose the handler on a private
// admin port — see future work below).
//
// PATH PREFIX
// -----------
// Standard library `net/http/pprof` registers under
// `/debug/pprof/`. We honour that path so off-the-shelf
// tooling (`go tool pprof <url>`) works without extra args.
//
// CORRELATION WITH RATE LIMITER + AUTH
// ------------------------------------
// `/debug/pprof/*` does NOT live under `/api`, so:
//   - the auth middleware skips it (it only intercepts /api),
//   - the request logger skips it (same gate),
//   - the rate limiter does NOT skip it explicitly, but the
//     pprof endpoints are read-class and very low-traffic in
//     practice; the lenient read bucket (50 RPS, burst 100) is
//     more than enough for a single SRE running pprof.
//
// FUTURE WORK
// -----------
//   - Optional separate admin port (e.g. `:6060`), so the
//     pprof endpoints aren't reachable from the public ingress
//     even when enabled. The same port could host /debug/vars,
//     custom dump endpoints, etc.
//   - Token-gated handler — accept requests only if a
//     time-limited admin token is in the Authorization header.

package main

import (
	"net/http"
	"net/http/pprof"
	"os"
	"strings"
)

// pprofEnabled reports whether the runtime profiling endpoints
// should be registered. Reads `PPROF_ENABLED` and accepts the
// usual truthy values.
func pprofEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("PPROF_ENABLED")))
	switch v {
	case "1", "true", "t", "yes", "y", "on":
		return true
	}
	return false
}

// registerPprof attaches the standard set of profiling handlers
// to the provided mux under `/debug/pprof/`. No-op when
// PPROF_ENABLED is unset or falsy.
//
// We register the handlers explicitly rather than relying on
// `import _ "net/http/pprof"`'s side-effect on http.DefaultServeMux,
// because the app uses a private `*http.ServeMux` and the
// blank-import approach would attach pprof to a mux we never
// serve.
func registerPprof(mux *http.ServeMux) bool {
	if !pprofEnabled() {
		return false
	}

	// Index serves the directory listing of available profiles
	// at /debug/pprof/. A trailing slash is required by the
	// stdlib handler for the index to work; we register both
	// shapes so curl users without the slash get a usable
	// response too.
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.Handle("/debug/pprof", http.RedirectHandler("/debug/pprof/", http.StatusMovedPermanently))

	// Sub-handlers for the named profiles. The stdlib's
	// `pprof.Index` would dispatch these via the generic
	// handler, but registering them explicitly gives a faster
	// path and means the routes show up in `mux` for anyone
	// listing handlers.
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	return true
}

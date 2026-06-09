// Package masters embeds the 10 international master persona JSON
// templates and exposes them as in-memory bytes.
//
// We intentionally keep this package tiny — a single Go file with
// nothing but the embed directive and a reader API. The richer
// MasterPersona type lives in internal/agent so we don't pull the
// embed into every test of the agent package.
//
// Why embed rather than os.ReadFile at startup:
//   * the JSON templates ship with the binary; ops can't accidentally
//     deploy a server without them
//   * eliminates a working-directory landmine in tests
//   * keeps the loader trivially mockable (override the FS in tests)
package masters

import "embed"

// FS is the embedded filesystem holding every <key>.json under
// this directory. Filenames double as the master key — the loader
// in internal/agent strips the .json suffix to derive the agent_id.
//
//go:embed *.json
var FS embed.FS

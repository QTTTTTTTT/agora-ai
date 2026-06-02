package modelab

import (
	"crypto/sha256"
	"encoding/binary"
)

// PickArm returns the arm index a (run, step, agent) tuple maps
// to, given the experiment's traffic_split. The output is stable
// across processes because we use SHA-256 → uint64 modulo
// (sum_of_weights × 1e6) → cumulative-bucket lookup.
//
// Why hash, not RNG: we need (a) sticky arms within a workflow
// (same tuple → same arm even on a re-run), AND (b) reproducible
// arm picks across replicas of the same orchestrator. crypto/sha256
// is fast enough on the hot path (≤1µs) and gives us a uniformly
// distributed key without needing a seed source.
//
// Why include agent_id in the key: a single workflow_run hits
// many agents (PM + 4 analysts + 3 debate personas). Hashing only
// (run, step) would force every agent's call to fall into the
// same arm, which is fine for "step-level" experiments but
// pointless for "compare PM-agent A vs PM-agent B". Including
// agent_id keeps the experiment scope (which the Match function
// already filters by) and the bucketing key orthogonal.
func PickArm(runID, step, agentID string, trafficSplit []float64) int {
	if len(trafficSplit) == 0 {
		return 0
	}
	if len(trafficSplit) == 1 {
		return 0
	}
	// Build the cumulative-weight ladder with a 1e6 fixed-point
	// scale so we can hash into uint64 without floating-point
	// drift. Sum is clamped at the end to handle small validation
	// tolerances (e.g. 0.33+0.33+0.33 = 0.99).
	const scale = 1_000_000
	cum := make([]uint64, len(trafficSplit))
	var total uint64
	for i, w := range trafficSplit {
		if w < 0 {
			w = 0
		}
		total += uint64(w * scale)
		cum[i] = total
	}
	if total == 0 {
		return 0
	}

	h := sha256.Sum256([]byte(runID + "|" + step + "|" + agentID))
	bucket := binary.BigEndian.Uint64(h[:8]) % total
	for i, c := range cum {
		if bucket < c {
			return i
		}
	}
	return len(trafficSplit) - 1
}

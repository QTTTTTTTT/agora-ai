package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// chainEncodingVersion is the on-disk format version for the
// canonical encoding hashed into row_hash. Bump it (and KEEP the
// older encoders available behind a switch in chainEncodeAccess /
// chainEncodeMutation) whenever the metadata fields included in the
// hash change. The verifier replays both encodings during a
// migration window so already-written rows keep verifying after the
// version bump.
const chainEncodingVersion = 1

// chainGenesisHash is the byte string used as the "previous" hash
// for the genesis row of a chain. Storing 32 zero bytes (vs NULL)
// keeps every row_hash computation uniform — the genesis row is no
// longer a special case in the hashing path. The DB still stores
// NULL in prev_hash for the genesis row so SQL queries can identify
// it cheaply.
var chainGenesisHash = make([]byte, 32)

// canonicalAccessEvent is the field-ordered struct that gets
// JSON-encoded into the canonical bytes hashed into
// data_access_log.row_hash. The encoder uses sorted-keys (ensured
// implicitly by the struct field order at marshal time) so
// re-running it on the same logical event always produces the same
// bytes.
//
// CHANGES TO THE FIELDS BREAK CHAIN VERIFICATION FOR EVERY ROW IN
// THE DATABASE. Bump chainEncodingVersion + add a v=N branch to
// chainEncodeAccess.
type canonicalAccessEvent struct {
	V            int    `json:"v"`
	Prev         string `json:"prev"`
	ID           string `json:"id"`
	Actor        string `json:"actor"`
	Action       string `json:"action"`
	ResourceType string `json:"resource_type"`
	ResourceID   string `json:"resource_id"`
	DetailsHash  string `json:"details_sha256"`
	CreatedAtNs  int64  `json:"created_at_ns"`
}

// canonicalMutationEvent is the same idea for admin_change_log.
type canonicalMutationEvent struct {
	V            int    `json:"v"`
	Prev         string `json:"prev"`
	ID           string `json:"id"`
	Actor        string `json:"actor"`
	Action       string `json:"action"`
	TargetType   string `json:"target_type"`
	TargetID     string `json:"target_id"`
	RequestID    string `json:"request_id"`
	BeforeHash   string `json:"before_sha256"`
	AfterHash    string `json:"after_sha256"`
	MetadataHash string `json:"metadata_sha256"`
	CreatedAtNs  int64  `json:"created_at_ns"`
}

// computeAccessRowHash returns the row_hash bytes for one
// data_access_log row.
func computeAccessRowHash(prev []byte, id, actor, action, resourceType, resourceID string, detailsHash []byte, createdAt time.Time) ([]byte, error) {
	if len(prev) == 0 {
		prev = chainGenesisHash
	}
	ev := canonicalAccessEvent{
		V:            chainEncodingVersion,
		Prev:         hex.EncodeToString(prev),
		ID:           id,
		Actor:        actor,
		Action:       action,
		ResourceType: resourceType,
		ResourceID:   resourceID,
		DetailsHash:  hex.EncodeToString(detailsHash),
		CreatedAtNs:  createdAt.UTC().UnixNano(),
	}
	bytes, err := json.Marshal(ev)
	if err != nil {
		return nil, fmt.Errorf("audit: encode access event: %w", err)
	}
	sum := sha256.Sum256(bytes)
	return sum[:], nil
}

// computeMutationRowHash returns the row_hash bytes for one
// admin_change_log row.
func computeMutationRowHash(prev []byte, id, actor, action, targetType, targetID, requestID string, beforeHash, afterHash, metadataHash []byte, createdAt time.Time) ([]byte, error) {
	if len(prev) == 0 {
		prev = chainGenesisHash
	}
	ev := canonicalMutationEvent{
		V:            chainEncodingVersion,
		Prev:         hex.EncodeToString(prev),
		ID:           id,
		Actor:        actor,
		Action:       action,
		TargetType:   targetType,
		TargetID:     targetID,
		RequestID:    requestID,
		BeforeHash:   hex.EncodeToString(beforeHash),
		AfterHash:    hex.EncodeToString(afterHash),
		MetadataHash: hex.EncodeToString(metadataHash),
		CreatedAtNs:  createdAt.UTC().UnixNano(),
	}
	bytes, err := json.Marshal(ev)
	if err != nil {
		return nil, fmt.Errorf("audit: encode mutation event: %w", err)
	}
	sum := sha256.Sum256(bytes)
	return sum[:], nil
}

// hashCanonicalJSON returns the sha256 hash of the canonical-form
// encoding of value. The canonical form is JSON with map keys sorted
// recursively at every level. This makes the hash deterministic
// across:
//
//   * Go map iteration order
//   * Postgres JSONB round-trip (which preserves all data but may
//     re-order keys at storage time)
//   * client serialisations (so a long-running fund frontend that
//     POSTs metadata in different orders still produces the same
//     audit fingerprint)
//
// Returns a nil byte slice for nil / empty inputs so the caller can
// store NULL details_hash for empty payloads.
func hashCanonicalJSON(raw json.RawMessage) ([]byte, error) {
	if isEmptyJSON(raw) {
		return nil, nil
	}
	canonical, err := canonicalJSON(raw)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(canonical)
	return sum[:], nil
}

// hashCanonicalAny is a convenience wrapper that marshals an
// arbitrary value first, then canonicalises and hashes.
func hashCanonicalAny(v any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("audit: marshal canonical: %w", err)
	}
	return hashCanonicalJSON(b)
}

// isEmptyJSON reports whether raw is empty (nil, length zero,
// "null", "{}", "[]", or empty after trimming whitespace). These
// all hash to the same "no payload" state to avoid a chain
// distinguishing semantically-empty rows.
func isEmptyJSON(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	trimmed := trimJSONWhitespace(raw)
	if len(trimmed) == 0 {
		return true
	}
	switch string(trimmed) {
	case "null", "{}", "[]":
		return true
	}
	return false
}

// trimJSONWhitespace strips ASCII whitespace from both ends.
// Unlike strings.TrimSpace this avoids any UTF-8 decode cost.
func trimJSONWhitespace(b []byte) []byte {
	start, end := 0, len(b)
	for start < end {
		switch b[start] {
		case ' ', '\t', '\r', '\n':
			start++
			continue
		}
		break
	}
	for end > start {
		switch b[end-1] {
		case ' ', '\t', '\r', '\n':
			end--
			continue
		}
		break
	}
	return b[start:end]
}

// canonicalJSON re-encodes raw with map keys sorted at every level
// and no insignificant whitespace. The result is the byte string
// that gets sha256'd to produce a content hash.
//
// Implementation: decode into a generic any, walk the tree, sort
// every map's keys, re-encode. This is O(n log n) per call but
// audit payloads are small.
func canonicalJSON(raw json.RawMessage) ([]byte, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("audit: canonical decode: %w", err)
	}
	return marshalCanonical(v)
}

func marshalCanonical(v any) ([]byte, error) {
	switch val := v.(type) {
	case nil:
		return []byte("null"), nil
	case bool:
		if val {
			return []byte("true"), nil
		}
		return []byte("false"), nil
	case float64:
		// json.Unmarshal puts every number into float64; we re-encode
		// via json.Marshal so it matches the standard library's
		// float-to-string rules (smallest round-tripping form).
		return json.Marshal(val)
	case string:
		return json.Marshal(val)
	case []any:
		out := []byte{'['}
		for i, item := range val {
			if i > 0 {
				out = append(out, ',')
			}
			b, err := marshalCanonical(item)
			if err != nil {
				return nil, err
			}
			out = append(out, b...)
		}
		out = append(out, ']')
		return out, nil
	case map[string]any:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := []byte{'{'}
		for i, k := range keys {
			if i > 0 {
				out = append(out, ',')
			}
			kBytes, err := json.Marshal(k)
			if err != nil {
				return nil, err
			}
			out = append(out, kBytes...)
			out = append(out, ':')
			vBytes, err := marshalCanonical(val[k])
			if err != nil {
				return nil, err
			}
			out = append(out, vBytes...)
		}
		out = append(out, '}')
		return out, nil
	}
	// Fallback: encode through json.Marshal. Catches int64, time,
	// custom Marshallers, etc.
	return json.Marshal(v)
}

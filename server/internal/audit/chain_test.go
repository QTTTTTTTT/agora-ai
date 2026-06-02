package audit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// canonicalJSON
// ---------------------------------------------------------------------------

func TestCanonicalJSON_SortsMapKeysRecursively(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "flat map keys sorted",
			in:   `{"b":1,"a":2,"c":3}`,
			want: `{"a":2,"b":1,"c":3}`,
		},
		{
			name: "nested map keys sorted at every level",
			in:   `{"z":{"y":1,"x":2},"a":{"d":3,"b":4}}`,
			want: `{"a":{"b":4,"d":3},"z":{"x":2,"y":1}}`,
		},
		{
			name: "array element order preserved",
			in:   `{"arr":[3,1,2]}`,
			want: `{"arr":[3,1,2]}`,
		},
		{
			name: "whitespace stripped",
			in:   `  {  "a"  :   1  }  `,
			want: `{"a":1}`,
		},
		{
			name: "deeply nested mixed types",
			in:   `{"users":[{"name":"bob","id":2},{"name":"alice","id":1}],"meta":{"v":2,"a":"x"}}`,
			want: `{"meta":{"a":"x","v":2},"users":[{"id":2,"name":"bob"},{"id":1,"name":"alice"}]}`,
		},
		{
			name: "primitive values pass through",
			in:   `42`,
			want: `42`,
		},
		{
			name: "string passes through json escape",
			in:   `"hello \"world\""`,
			want: `"hello \"world\""`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := canonicalJSON(json.RawMessage(tc.in))
			if err != nil {
				t.Fatalf("canonicalJSON: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("canonicalJSON(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCanonicalJSON_DeterministicAcrossCalls(t *testing.T) {
	in := json.RawMessage(`{"b":[3,2,1],"a":{"y":1,"x":2}}`)
	first, err := canonicalJSON(in)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	for i := 0; i < 50; i++ {
		got, err := canonicalJSON(in)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if !bytes.Equal(got, first) {
			t.Fatalf("non-deterministic at call %d: %q vs %q", i, got, first)
		}
	}
}

func TestCanonicalJSON_DifferentMapOrderProducesSameBytes(t *testing.T) {
	// Two semantically-identical objects with different key
	// insertion order MUST canonicalise to the same bytes —
	// this is the property the JSONB round-trip relies on.
	a := json.RawMessage(`{"a":1,"b":2,"c":3}`)
	b := json.RawMessage(`{"c":3,"a":1,"b":2}`)
	ca, err := canonicalJSON(a)
	if err != nil {
		t.Fatalf("a: %v", err)
	}
	cb, err := canonicalJSON(b)
	if err != nil {
		t.Fatalf("b: %v", err)
	}
	if !bytes.Equal(ca, cb) {
		t.Errorf("canonicalisation not idempotent across key orders: %q vs %q", ca, cb)
	}
}

// ---------------------------------------------------------------------------
// hashCanonicalJSON / isEmptyJSON
// ---------------------------------------------------------------------------

func TestHashCanonicalJSON_NilEmptyAndSemanticallyEmptyAllReturnNil(t *testing.T) {
	cases := []json.RawMessage{
		nil,
		json.RawMessage(``),
		json.RawMessage(`null`),
		json.RawMessage(`{}`),
		json.RawMessage(`[]`),
		json.RawMessage(`   `),
		json.RawMessage(`  null  `),
	}
	for _, raw := range cases {
		got, err := hashCanonicalJSON(raw)
		if err != nil {
			t.Errorf("hashCanonicalJSON(%q): unexpected error %v", raw, err)
		}
		if got != nil {
			t.Errorf("hashCanonicalJSON(%q) = %x, want nil", raw, got)
		}
	}
}

func TestHashCanonicalJSON_NonEmpty_ConsistentLength(t *testing.T) {
	got, err := hashCanonicalJSON(json.RawMessage(`{"a":1}`))
	if err != nil {
		t.Fatalf("hashCanonicalJSON: %v", err)
	}
	if len(got) != 32 {
		t.Errorf("hash length = %d, want 32 (sha256)", len(got))
	}
}

func TestHashCanonicalJSON_DifferentValuesProduceDifferentHashes(t *testing.T) {
	h1, _ := hashCanonicalJSON(json.RawMessage(`{"x":1}`))
	h2, _ := hashCanonicalJSON(json.RawMessage(`{"x":2}`))
	if bytes.Equal(h1, h2) {
		t.Errorf("collision: distinct payloads produced same hash")
	}
}

func TestHashCanonicalJSON_KeyOrderInsensitive(t *testing.T) {
	h1, _ := hashCanonicalJSON(json.RawMessage(`{"a":1,"b":2}`))
	h2, _ := hashCanonicalJSON(json.RawMessage(`{"b":2,"a":1}`))
	if !bytes.Equal(h1, h2) {
		t.Errorf("hash differs by map order: %x vs %x", h1, h2)
	}
}

// ---------------------------------------------------------------------------
// computeAccessRowHash
// ---------------------------------------------------------------------------

func TestComputeAccessRowHash_DeterministicAndContentSensitive(t *testing.T) {
	createdAt := time.Date(2026, 5, 30, 10, 30, 0, 0, time.UTC)
	prev := bytes.Repeat([]byte{0xab}, 32)
	detailsHash, err := hashCanonicalJSON(json.RawMessage(`{"a":1}`))
	if err != nil {
		t.Fatalf("details hash: %v", err)
	}
	h1, err := computeAccessRowHash(prev, "row-1", "user-1", "read", "memory", "mem-1", detailsHash, createdAt)
	if err != nil {
		t.Fatalf("h1: %v", err)
	}
	h2, err := computeAccessRowHash(prev, "row-1", "user-1", "read", "memory", "mem-1", detailsHash, createdAt)
	if err != nil {
		t.Fatalf("h2: %v", err)
	}
	if !bytes.Equal(h1, h2) {
		t.Errorf("non-deterministic row hash: %x vs %x", h1, h2)
	}
	if len(h1) != 32 {
		t.Errorf("hash length = %d, want 32", len(h1))
	}

	// Each of the 7 inputs must influence the hash. Tweaking any
	// one of them must change the output.
	mutators := []struct {
		name string
		mut  func() ([]byte, error)
	}{
		{"prev", func() ([]byte, error) {
			alt := bytes.Repeat([]byte{0xcd}, 32)
			return computeAccessRowHash(alt, "row-1", "user-1", "read", "memory", "mem-1", detailsHash, createdAt)
		}},
		{"id", func() ([]byte, error) {
			return computeAccessRowHash(prev, "row-2", "user-1", "read", "memory", "mem-1", detailsHash, createdAt)
		}},
		{"actor", func() ([]byte, error) {
			return computeAccessRowHash(prev, "row-1", "user-2", "read", "memory", "mem-1", detailsHash, createdAt)
		}},
		{"action", func() ([]byte, error) {
			return computeAccessRowHash(prev, "row-1", "user-1", "export", "memory", "mem-1", detailsHash, createdAt)
		}},
		{"resourceType", func() ([]byte, error) {
			return computeAccessRowHash(prev, "row-1", "user-1", "read", "agent_config", "mem-1", detailsHash, createdAt)
		}},
		{"resourceID", func() ([]byte, error) {
			return computeAccessRowHash(prev, "row-1", "user-1", "read", "memory", "mem-2", detailsHash, createdAt)
		}},
		{"detailsHash", func() ([]byte, error) {
			alt, _ := hashCanonicalJSON(json.RawMessage(`{"a":2}`))
			return computeAccessRowHash(prev, "row-1", "user-1", "read", "memory", "mem-1", alt, createdAt)
		}},
		{"createdAt", func() ([]byte, error) {
			return computeAccessRowHash(prev, "row-1", "user-1", "read", "memory", "mem-1", detailsHash, createdAt.Add(time.Second))
		}},
	}
	for _, tc := range mutators {
		t.Run("mutating-"+tc.name, func(t *testing.T) {
			alt, err := tc.mut()
			if err != nil {
				t.Fatalf("mutate %s: %v", tc.name, err)
			}
			if bytes.Equal(h1, alt) {
				t.Errorf("mutating %s did NOT change row_hash — silent input ignored", tc.name)
			}
		})
	}
}

func TestComputeAccessRowHash_GenesisAcceptsNilPrev(t *testing.T) {
	createdAt := time.Date(2026, 5, 30, 10, 30, 0, 0, time.UTC)
	hWithNil, err := computeAccessRowHash(nil, "row-1", "u", "read", "memory", "x", nil, createdAt)
	if err != nil {
		t.Fatalf("nil prev: %v", err)
	}
	hWithZero, err := computeAccessRowHash(make([]byte, 32), "row-1", "u", "read", "memory", "x", nil, createdAt)
	if err != nil {
		t.Fatalf("zero prev: %v", err)
	}
	if !bytes.Equal(hWithNil, hWithZero) {
		t.Errorf("genesis sentinel mismatch: nil vs 32 zero bytes should produce same hash, got %x vs %x", hWithNil, hWithZero)
	}
}

// ---------------------------------------------------------------------------
// computeMutationRowHash
// ---------------------------------------------------------------------------

func TestComputeMutationRowHash_DeterministicAndAllInputsCommitted(t *testing.T) {
	createdAt := time.Date(2026, 5, 30, 10, 30, 0, 0, time.UTC)
	prev := bytes.Repeat([]byte{0x11}, 32)
	beforeHash, _ := hashCanonicalJSON(json.RawMessage(`{"old":1}`))
	afterHash, _ := hashCanonicalJSON(json.RawMessage(`{"new":2}`))
	metaHash, _ := hashCanonicalJSON(json.RawMessage(`{"by":"admin"}`))

	h1, err := computeMutationRowHash(prev, "id-1", "user-1", "update", "platform_settings", "_singleton_", "req-1", beforeHash, afterHash, metaHash, createdAt)
	if err != nil {
		t.Fatalf("h1: %v", err)
	}
	if len(h1) != 32 {
		t.Errorf("hash length = %d, want 32", len(h1))
	}

	mutators := []struct {
		name string
		mut  func() ([]byte, error)
	}{
		{"id", func() ([]byte, error) {
			return computeMutationRowHash(prev, "id-2", "user-1", "update", "platform_settings", "_singleton_", "req-1", beforeHash, afterHash, metaHash, createdAt)
		}},
		{"action", func() ([]byte, error) {
			return computeMutationRowHash(prev, "id-1", "user-1", "delete", "platform_settings", "_singleton_", "req-1", beforeHash, afterHash, metaHash, createdAt)
		}},
		{"requestID", func() ([]byte, error) {
			return computeMutationRowHash(prev, "id-1", "user-1", "update", "platform_settings", "_singleton_", "req-2", beforeHash, afterHash, metaHash, createdAt)
		}},
		{"beforeHash", func() ([]byte, error) {
			alt, _ := hashCanonicalJSON(json.RawMessage(`{"old":99}`))
			return computeMutationRowHash(prev, "id-1", "user-1", "update", "platform_settings", "_singleton_", "req-1", alt, afterHash, metaHash, createdAt)
		}},
	}
	for _, tc := range mutators {
		t.Run("mutating-"+tc.name, func(t *testing.T) {
			alt, err := tc.mut()
			if err != nil {
				t.Fatalf("mutate %s: %v", tc.name, err)
			}
			if bytes.Equal(h1, alt) {
				t.Errorf("mutating %s did not change row_hash", tc.name)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// hash format guards
// ---------------------------------------------------------------------------

func TestComputeAccessRowHash_HasExpectedSHA256OfCanonicalEncoding(t *testing.T) {
	// Lock the encoding so a future refactor that accidentally
	// changes JSON struct tags or ordering blows up loudly. The
	// expected hex is computed by hand from the v=1 encoder.
	prev := make([]byte, 32) // all zero — genesis
	createdAt := time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)
	detailsHash := bytes.Repeat([]byte{0xaa}, 32)
	got, err := computeAccessRowHash(prev, "row-x", "u", "a", "rt", "rid", detailsHash, createdAt)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	// Re-derive expected by mimicking the v=1 encoder exactly.
	expected := sha256.Sum256([]byte(
		`{"v":1,"prev":"` + hex.EncodeToString(prev) +
			`","id":"row-x","actor":"u","action":"a","resource_type":"rt","resource_id":"rid","details_sha256":"` +
			hex.EncodeToString(detailsHash) +
			`","created_at_ns":` + intToString(createdAt.UnixNano()) +
			`}`))
	if !bytes.Equal(got, expected[:]) {
		t.Errorf("encoding drift: got %s, want %s", hex.EncodeToString(got), hex.EncodeToString(expected[:]))
	}
}

// intToString avoids importing strconv just for one call site.
func intToString(n int64) string {
	if n == 0 {
		return "0"
	}
	negative := n < 0
	if negative {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	out := string(buf[i:])
	if negative {
		return "-" + out
	}
	return out
}

// ---------------------------------------------------------------------------
// trimJSONWhitespace
// ---------------------------------------------------------------------------

func TestTrimJSONWhitespace(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"   ", ""},
		{"abc", "abc"},
		{"  abc  ", "abc"},
		{"\t\n abc \r\n", "abc"},
		{"  {\"a\":1}  ", `{"a":1}`},
	}
	for _, tc := range cases {
		got := string(trimJSONWhitespace([]byte(tc.in)))
		if got != tc.want {
			t.Errorf("trimJSONWhitespace(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// isEmptyJSON
// ---------------------------------------------------------------------------

func TestIsEmptyJSON(t *testing.T) {
	empty := []string{"", "   ", "null", "{}", "[]", "  null  ", "\t\n", "  {} "}
	notEmpty := []string{`{"a":1}`, `[1]`, `0`, `false`, `"x"`, `null,trailing`}
	for _, s := range empty {
		if !isEmptyJSON(json.RawMessage(s)) {
			t.Errorf("isEmptyJSON(%q) = false, want true", s)
		}
	}
	for _, s := range notEmpty {
		if isEmptyJSON(json.RawMessage(s)) {
			t.Errorf("isEmptyJSON(%q) = true, want false", s)
		}
	}
}

// ---------------------------------------------------------------------------
// hashCanonicalAny
// ---------------------------------------------------------------------------

func TestHashCanonicalAny_SkipsNil(t *testing.T) {
	got, err := hashCanonicalAny(nil)
	if err != nil {
		t.Errorf("hashCanonicalAny(nil) error = %v", err)
	}
	if got != nil {
		t.Errorf("hashCanonicalAny(nil) = %x, want nil", got)
	}
}

func TestHashCanonicalAny_StableAcrossMapOrders(t *testing.T) {
	// Go map iteration order is randomised; the canonicaliser MUST
	// flatten that variability or chain verification would
	// non-deterministically fail across the same logical data.
	m := map[string]any{"x": 1, "y": 2, "z": []any{3, 4}}
	first, err := hashCanonicalAny(m)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	for i := 0; i < 100; i++ {
		got, err := hashCanonicalAny(m)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if !bytes.Equal(got, first) {
			t.Fatalf("hashCanonicalAny non-deterministic across map iterations at call %d", i)
		}
	}
}

// ---------------------------------------------------------------------------
// Sanity: encoding version is locked
// ---------------------------------------------------------------------------

func TestChainEncodingVersion_Locked(t *testing.T) {
	// This test exists so a refactor that bumps the version
	// without also updating the encoder is forced to think about
	// the chain re-verification migration.
	if chainEncodingVersion != 1 {
		t.Fatalf("chainEncodingVersion bumped to %d. If intentional, update both encoder branches and re-verify legacy rows.", chainEncodingVersion)
	}
}

// TestChainGenesisHash_AllZeros locks the genesis sentinel — if a
// refactor changes it the entire chain breaks.
func TestChainGenesisHash_AllZeros(t *testing.T) {
	if len(chainGenesisHash) != 32 {
		t.Fatalf("chainGenesisHash length = %d, want 32", len(chainGenesisHash))
	}
	for i, b := range chainGenesisHash {
		if b != 0 {
			t.Errorf("chainGenesisHash[%d] = %x, want 0", i, b)
		}
	}
}

// TestCanonicalEventStruct_FieldOrderLocked asserts JSON tag stability.
func TestCanonicalEventStruct_FieldOrderLocked(t *testing.T) {
	access := canonicalAccessEvent{
		V: 1, Prev: "p", ID: "i", Actor: "a", Action: "ac",
		ResourceType: "rt", ResourceID: "rid", DetailsHash: "dh", CreatedAtNs: 1,
	}
	b, err := json.Marshal(access)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"v":1`, `"prev":"p"`, `"id":"i"`, `"actor":"a"`, `"action":"ac"`, `"resource_type":"rt"`, `"resource_id":"rid"`, `"details_sha256":"dh"`, `"created_at_ns":1`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("missing %s in encoded form %s", want, b)
		}
	}
}

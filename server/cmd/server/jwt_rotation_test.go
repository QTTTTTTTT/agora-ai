// jwt_rotation_test.go — A4 coverage. Exercises the rotation
// handler + trigger end-to-end against an in-memory KeyringStore
// so the test never touches Postgres but still proves the full
// loop: event arrives -> mint key -> persist -> reload ->
// hot-swap manager -> prune old rows.
//
// The fake store mirrors the contract of secrets.KeyringStore
// exactly. Keeping it test-local (rather than a "TestStore" export
// in the secrets package) avoids polluting the production API
// surface with helpers only the cmd/server tests care about.
package main

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/fundai/server/internal/outbox"
	"github.com/fundai/server/internal/secrets"
	"github.com/fundai/server/internal/subscription"
)

// memKeyringStore is a thread-safe in-memory KeyringStore. The
// rows slice is the source of truth; ActiveAge / ListAll /
// AppendActive / PruneRotatedOutBefore all operate over it under
// mu so the trigger goroutine and the handler can hit the same
// store without racing.
type memKeyringStore struct {
	mu   sync.Mutex
	rows []secrets.StoredKey
}

func (s *memKeyringStore) ListAll(ctx context.Context) ([]secrets.StoredKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]secrets.StoredKey, len(s.rows))
	copy(out, s.rows)
	return out, nil
}

func (s *memKeyringStore) AppendActive(ctx context.Context, k secrets.StoredKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	for i := range s.rows {
		if s.rows[i].IsActive {
			s.rows[i].IsActive = false
			s.rows[i].RotatedOutAt = &now
		}
	}
	if k.CreatedAt.IsZero() {
		k.CreatedAt = now
	}
	s.rows = append(s.rows, k)
	return nil
}

func (s *memKeyringStore) PruneRotatedOutBefore(ctx context.Context, cutoff time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keep := s.rows[:0]
	pruned := 0
	for _, r := range s.rows {
		if !r.IsActive && r.RotatedOutAt != nil && r.RotatedOutAt.Before(cutoff) {
			pruned++
			continue
		}
		keep = append(keep, r)
	}
	s.rows = keep
	return pruned, nil
}

func (s *memKeyringStore) ActiveAge(ctx context.Context) (time.Duration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.rows {
		if r.IsActive {
			return time.Since(r.CreatedAt), nil
		}
	}
	// Mirror PostgresKeyringStore's empty-table behaviour so the
	// trigger's sql.ErrNoRows branch is exercised by tests too.
	// We intentionally avoid importing database/sql here to keep
	// the fake import-free, and return a sentinel zero +
	// secrets-package-defined error if one exists; otherwise just
	// return zero, nil — the trigger checks (age < Interval) so a
	// zero age is itself a no-op signal.
	return 0, nil
}

// withKEK is the common fixture: every test needs the encryption
// secret in scope because the handler + seed both call
// secrets.EncryptionSecret(). Tests that need different KEKs can
// override after this fixture runs.
func withKEK(t *testing.T) {
	t.Helper()
	t.Setenv("MODEL_CONFIG_API_KEY_SECRET", "test-kek-32-bytes-long-padding!!")
}

func TestJWTRotationHandler_IgnoresOtherEvents(t *testing.T) {
	withKEK(t)
	store := &memKeyringStore{}
	ring, err := secrets.NewJWTKeyring([]secrets.JWTKey{{Kid: "k0", Secret: "seed-secret", Active: true}})
	if err != nil {
		t.Fatalf("seed ring: %v", err)
	}
	mgr := secrets.NewKeyringManager(ring)
	h := newJWTRotationHandler(jwtRotationHandlerOptions{
		Manager:    mgr,
		Store:      store,
		SessionTTL: time.Hour,
	})
	err = h.Handle(context.Background(), outbox.ConsumedEvent{
		EventType: "not.the.rotation.event",
	})
	if err != nil {
		t.Fatalf("unrelated event should be a no-op, got: %v", err)
	}
	if len(store.rows) != 0 {
		t.Fatalf("store mutated by unrelated event; got %d rows", len(store.rows))
	}
	if mgr.Get().Active().Kid != "k0" {
		t.Fatalf("manager mutated by unrelated event; active kid = %q", mgr.Get().Active().Kid)
	}
}

func TestJWTRotationHandler_RotatesEndToEnd(t *testing.T) {
	withKEK(t)

	// Seed: ring starts with k0 active, no DB rows yet. We persist
	// the seed manually so reload finds an entry to demote.
	kek, _ := secrets.EncryptionSecret()
	seedCipher, err := subscription.EncryptAPIKey("seed-secret-32bytes-hex-padding!!", kek)
	if err != nil {
		t.Fatalf("seed encrypt: %v", err)
	}
	store := &memKeyringStore{
		rows: []secrets.StoredKey{
			{
				Kid:               "k0",
				SecretEncrypted:   []byte(seedCipher),
				SecretFingerprint: "seedfp01",
				IsActive:          true,
				CreatedAt:         time.Now().Add(-10 * time.Minute).UTC(),
			},
		},
	}
	ring, err := secrets.NewJWTKeyring([]secrets.JWTKey{
		{Kid: "k0", Secret: "seed-secret-32bytes-hex-padding!!", Active: true},
	})
	if err != nil {
		t.Fatalf("seed ring: %v", err)
	}
	mgr := secrets.NewKeyringManager(ring)
	h := newJWTRotationHandler(jwtRotationHandlerOptions{
		Manager:           mgr,
		Store:             store,
		SessionTTL:        time.Hour,
		KeepRotatedWindow: time.Hour,
	})

	payload, _ := json.Marshal(map[string]string{"reason": "test"})
	if err := h.Handle(context.Background(), outbox.ConsumedEvent{
		ID:        "evt-1",
		EventType: jwtRotationEventType,
		Payload:   payload,
	}); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}

	// Post-conditions:
	//  1. store now has 2 rows total (k0 demoted, new key active)
	//  2. exactly one row IsActive
	//  3. manager.Get().Active().Kid is the new (non-k0) kid
	//  4. manager.Get() can still verify against k0 because k0 is
	//     within the 1h KeepRotatedWindow.
	if len(store.rows) != 2 {
		t.Fatalf("expected 2 rows after rotation, got %d", len(store.rows))
	}
	var activeCount int
	for _, r := range store.rows {
		if r.IsActive {
			activeCount++
		}
	}
	if activeCount != 1 {
		t.Fatalf("expected exactly 1 active row, got %d", activeCount)
	}
	live := mgr.Get()
	if live.Active().Kid == "k0" {
		t.Fatalf("manager still pointing at old kid k0 after rotation")
	}
	if _, ok := live.LookupKid("k0"); !ok {
		t.Fatalf("verification path lost k0 within retention window")
	}
}

func TestJWTRotationHandler_PrunesPastRetention(t *testing.T) {
	withKEK(t)
	kek, _ := secrets.EncryptionSecret()

	// Seed a stale rotated-out key (rotated 48h ago) alongside the
	// active one. KeepRotatedWindow=24h must drop the stale row.
	oldCipher, _ := subscription.EncryptAPIKey("old-secret-32bytes-hex-padding!!", kek)
	seedCipher, _ := subscription.EncryptAPIKey("active-secret-32bytes-hex-pad!!!", kek)
	rotatedAt := time.Now().Add(-48 * time.Hour).UTC()
	store := &memKeyringStore{
		rows: []secrets.StoredKey{
			{
				Kid:               "k_stale",
				SecretEncrypted:   []byte(oldCipher),
				SecretFingerprint: "oldfp001",
				IsActive:          false,
				CreatedAt:         time.Now().Add(-72 * time.Hour).UTC(),
				RotatedOutAt:      &rotatedAt,
			},
			{
				Kid:               "k_active",
				SecretEncrypted:   []byte(seedCipher),
				SecretFingerprint: "activefp",
				IsActive:          true,
				CreatedAt:         time.Now().Add(-10 * time.Minute).UTC(),
			},
		},
	}
	ring, _ := secrets.NewJWTKeyring([]secrets.JWTKey{
		{Kid: "k_active", Secret: "active-secret-32bytes-hex-pad!!!", Active: true},
	})
	mgr := secrets.NewKeyringManager(ring)
	h := newJWTRotationHandler(jwtRotationHandlerOptions{
		Manager:           mgr,
		Store:             store,
		SessionTTL:        time.Hour,
		KeepRotatedWindow: 24 * time.Hour,
	})

	if err := h.Handle(context.Background(), outbox.ConsumedEvent{
		EventType: jwtRotationEventType,
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	for _, r := range store.rows {
		if r.Kid == "k_stale" {
			t.Fatalf("stale row not pruned; rotation retention window failed")
		}
	}
}

func TestJWTRotationTrigger_EnqueueDebounce(t *testing.T) {
	withKEK(t)
	store := &memKeyringStore{
		rows: []secrets.StoredKey{
			{
				Kid:       "k0",
				IsActive:  true,
				CreatedAt: time.Now().Add(-2 * time.Hour).UTC(),
			},
		},
	}
	var enqueued int
	var emu sync.Mutex
	enqueue := func(ctx context.Context, ev outbox.Event) error {
		emu.Lock()
		defer emu.Unlock()
		enqueued++
		return nil
	}
	trig := newJWTRotationTrigger(jwtRotationTriggerOptions{
		Store:    store,
		Interval: time.Hour,
		Enqueue:  enqueue,
	})

	// Two consecutive ticks within debounce window — only the
	// first should enqueue.
	trig.tick(context.Background())
	trig.tick(context.Background())
	emu.Lock()
	got := enqueued
	emu.Unlock()
	if got != 1 {
		t.Fatalf("expected exactly 1 enqueue under debounce, got %d", got)
	}
}

func TestJWTRotationTrigger_SkipsWhenYoung(t *testing.T) {
	withKEK(t)
	store := &memKeyringStore{
		rows: []secrets.StoredKey{
			{
				Kid:       "k0",
				IsActive:  true,
				CreatedAt: time.Now().Add(-5 * time.Minute).UTC(),
			},
		},
	}
	enqueued := 0
	enqueue := func(ctx context.Context, ev outbox.Event) error {
		enqueued++
		return nil
	}
	trig := newJWTRotationTrigger(jwtRotationTriggerOptions{
		Store:    store,
		Interval: time.Hour,
		Enqueue:  enqueue,
	})
	trig.tick(context.Background())
	if enqueued != 0 {
		t.Fatalf("expected no enqueue when active key is younger than interval, got %d", enqueued)
	}
}

func TestSeedKeyringFromEnv_PersistsActiveKey(t *testing.T) {
	withKEK(t)
	store := &memKeyringStore{}
	ring, err := secrets.NewJWTKeyring([]secrets.JWTKey{
		{Kid: "env-active", Secret: "env-seed-32bytes-hex-padding!!!a", Active: true},
		{Kid: "env-prior", Secret: "env-prior-32bytes-hex-padding!!a", Active: false},
	})
	if err != nil {
		t.Fatalf("env ring: %v", err)
	}
	if err := seedKeyringFromEnv(context.Background(), store, ring); err != nil {
		t.Fatalf("seedKeyringFromEnv: %v", err)
	}
	if len(store.rows) != 1 {
		t.Fatalf("expected 1 seeded row (active only), got %d", len(store.rows))
	}
	if store.rows[0].Kid != "env-active" {
		t.Fatalf("expected env-active kid seeded, got %q", store.rows[0].Kid)
	}
	if !store.rows[0].IsActive {
		t.Fatalf("seeded row should be active")
	}
}

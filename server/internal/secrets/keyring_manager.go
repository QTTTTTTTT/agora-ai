// keyring_manager.go — A4 atomic hot-swap envelope around JWTKeyring.
//
// Until A4 the live JWTKeyring was captured once in cfg.JWTKeyring
// and that pointer never moved. Every signing / verification path
// dereferenced it directly. After A4 the rotation handler needs to
// publish a NEW keyring into every caller's view without forcing a
// process restart — so we wrap the pointer in atomic.Pointer and
// expose Get / Swap.
//
// The wrapper is intentionally tiny: no metrics, no rotation
// policy, no logging. Those concerns live in the outbox handler.
// The wrapper exists purely so that
//
//   cfg.KeyringManager.Get()
//
// is the single point where production code reads the live ring,
// and so that a rotation can call
//
//   cfg.KeyringManager.Swap(newRing)
//
// without a mutex roundtrip on every JWT verification (atomic
// load is a single instruction on x86 / arm).
//
// Why not a sync.RWMutex
//
//   - JWT verification is on the hot path of every authenticated
//     request. A mutex (even RLock) adds a global serialization
//     point that produces sysmon-visible contention on busy
//     deployments. atomic.Pointer is wait-free.
//   - The ring is immutable after construction — once a
//     *JWTKeyring is published, nothing inside it changes. So
//     the only thing we need to synchronise is the pointer
//     swap itself, which is exactly what atomic.Pointer was
//     designed for.

package secrets

import "sync/atomic"

// KeyringManager publishes a live *JWTKeyring that callers read
// via Get() and a rotation publishes via Swap(). Nil-safe at
// every method so test boots that wire a no-op manager don't
// have to special-case it at every call site.
type KeyringManager struct {
	ring atomic.Pointer[JWTKeyring]
}

// NewKeyringManager seeds the manager with the boot-time ring.
// Pass nil here only in tests that don't exercise the JWT path —
// production wiring must pass a fully-constructed ring or
// every authenticated request will see nil.
func NewKeyringManager(initial *JWTKeyring) *KeyringManager {
	m := &KeyringManager{}
	m.ring.Store(initial)
	return m
}

// Get returns the currently published ring. Nil when the
// manager hasn't been seeded yet — callers should treat that
// case as "keyring not configured" and refuse to sign or
// verify, identical to the pre-A4 nil-cfg.JWTKeyring path.
func (m *KeyringManager) Get() *JWTKeyring {
	if m == nil {
		return nil
	}
	return m.ring.Load()
}

// Swap atomically replaces the published ring. Used by the
// rotation outbox handler after it has minted a new key and
// persisted the new full set. Returns the prior ring so the
// caller can log "rotated kid X → kid Y" without a separate
// Get() race window.
func (m *KeyringManager) Swap(next *JWTKeyring) *JWTKeyring {
	if m == nil {
		return nil
	}
	return m.ring.Swap(next)
}

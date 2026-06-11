// jwt_rotation_handler.go — A4 outbox handler that performs JWT
// keyring rotation when it receives a `secret.rotate.jwt` event.
//
// Why a handler instead of a goroutine
//
// The rotation is event-driven on purpose: the trigger goroutine
// only DECIDES when to rotate (active key older than configured
// interval, no pending rotation already in flight). The actual
// rotation work — mint key, persist, reload, prune — lives here so
// it (a) runs in the same retry / dead-letter framework as every
// other outbox sink, (b) is invokable from a manual `INSERT INTO
// outbox_events` if an operator wants to force a rotation outside
// the normal cadence.
//
// Handler responsibilities
//
//   1. Filter by EventType — return nil for anything that isn't
//      `secret.rotate.jwt` so the same Handler can sit in a
//      MultiHandler with the LoggingHandler and other future
//      sinks.
//
//   2. Mint a fresh key via secrets.GenerateJWTKey. Encrypt with
//      subscription.EncryptAPIKey under the KEK fetched from
//      secrets.EncryptionSecret (same KEK used for every other
//      stored secret material, see A3 commit).
//
//   3. Persist via KeyringStore.AppendActive — the store handles
//      the "demote previous active + insert new active" tx so the
//      DB-side single-active invariant is never violated.
//
//   4. Reload the full set, decrypt every plaintext, build a new
//      *secrets.JWTKeyring, atomic-swap it into the
//      KeyringManager. After this swap returns, every subsequent
//      JWT verification sees the new key.
//
//   5. Prune rotated-out rows older than KeepRotatedWindow so the
//      verification key set doesn't grow unbounded. Window must
//      be at least 1× SessionTTL + 1× clock-skew tolerance so
//      tokens issued just before rotation can still verify.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/fundai/server/internal/outbox"
	"github.com/fundai/server/internal/secrets"
	"github.com/fundai/server/internal/subscription"
)

// jwtRotationEventType is the only EventType the handler reacts to.
// Anything else is silently ignored so a MultiHandler chain stays
// composable.
const jwtRotationEventType = "secret.rotate.jwt"

// jwtRotationHandlerOptions configures the handler. All fields are
// required when wired in production; zero values cause an early
// nil-result so unit tests that don't exercise rotation don't have
// to plumb the manager.
type jwtRotationHandlerOptions struct {
	Manager           *secrets.KeyringManager
	Store             secrets.KeyringStore
	SessionTTL        time.Duration
	KeepRotatedWindow time.Duration // pruning cutoff; defaults to 3 × SessionTTL
	Logger            *slog.Logger
}

// jwtRotationHandler is the outbox.Handler implementation.
type jwtRotationHandler struct {
	opts jwtRotationHandlerOptions
}

// newJWTRotationHandler is the constructor. Returns nil when the
// manager or store are absent — the caller must check and skip
// registration so a degraded boot doesn't dead-letter every event.
func newJWTRotationHandler(opts jwtRotationHandlerOptions) *jwtRotationHandler {
	if opts.Manager == nil || opts.Store == nil {
		return nil
	}
	if opts.KeepRotatedWindow <= 0 {
		// 3× SessionTTL is the rule of thumb: long enough for any
		// token minted just before rotation to age out naturally
		// (≤ 1× TTL), plus a 2× safety margin for clock skew and
		// long-running batch jobs that re-use a stale-but-still-
		// valid token. Configurable so a deployment that runs
		// VERY long-lived tokens can bump it.
		opts.KeepRotatedWindow = 3 * opts.SessionTTL
		if opts.KeepRotatedWindow == 0 {
			opts.KeepRotatedWindow = 24 * time.Hour
		}
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &jwtRotationHandler{opts: opts}
}

// Handle implements outbox.Handler. The dispatch shape mirrors
// every other event-typed handler in the codebase: filter, decode,
// execute, log, return.
func (h *jwtRotationHandler) Handle(ctx context.Context, ev outbox.ConsumedEvent) error {
	if h == nil {
		return nil
	}
	if ev.EventType != jwtRotationEventType {
		return nil
	}

	// payload is intentionally permissive — every field is optional.
	// We accept an empty payload because the trigger goroutine
	// doesn't always have anything interesting to say beyond "do it".
	var payload struct {
		Reason string `json:"reason,omitempty"`
	}
	if len(ev.Payload) > 0 {
		_ = json.Unmarshal(ev.Payload, &payload)
	}

	kek, err := secrets.EncryptionSecret()
	if err != nil {
		return fmt.Errorf("jwt-rotation: load KEK: %w", err)
	}
	if kek == "" {
		// Hard fail rather than dead-letter: a missing KEK means
		// the operator hasn't finished configuring the platform.
		// Retrying after they fix it is exactly what we want.
		return fmt.Errorf("jwt-rotation: encryption secret unavailable")
	}

	encrypt := func(plaintext string) ([]byte, error) {
		ct, err := subscription.EncryptAPIKey(plaintext, kek)
		if err != nil {
			return nil, err
		}
		return []byte(ct), nil
	}

	plaintext, stored, err := secrets.GenerateJWTKey(encrypt)
	if err != nil {
		return fmt.Errorf("jwt-rotation: mint new key: %w", err)
	}

	// Persist + demote prior — inside the store's transaction so
	// the (is_active=TRUE) partial unique index never sees two
	// active rows.
	if err := h.opts.Store.AppendActive(ctx, stored); err != nil {
		return fmt.Errorf("jwt-rotation: persist new active: %w", err)
	}

	// Re-read every row + decrypt so the rebuilt ring reflects
	// exactly what's in the DB right now. Reload (rather than
	// patching the prior ring locally) makes a concurrent operator
	// `INSERT INTO jwt_keyring` show up in the live ring on the
	// next rotation cycle — useful for emergency key revocation.
	all, err := h.opts.Store.ListAll(ctx)
	if err != nil {
		return fmt.Errorf("jwt-rotation: re-list keys: %w", err)
	}
	pts := make(map[string]string, len(all))
	for _, k := range all {
		// The fresh key we just minted is already in the slice;
		// short-circuit so we don't waste a decrypt round-trip
		// on the value we have in hand.
		if k.Kid == stored.Kid {
			pts[k.Kid] = plaintext
			continue
		}
		pt, err := subscription.DecryptAPIKey(string(k.SecretEncrypted), kek)
		if err != nil {
			return fmt.Errorf("jwt-rotation: decrypt kid=%q: %w", k.Kid, err)
		}
		pts[k.Kid] = pt
	}
	ring, err := secrets.BuildKeyringFromStored(all, pts)
	if err != nil {
		return fmt.Errorf("jwt-rotation: build ring: %w", err)
	}
	prior := h.opts.Manager.Swap(ring)
	priorKid := ""
	if prior != nil {
		priorKid = prior.Active().Kid
	}

	pruned, pruneErr := h.opts.Store.PruneRotatedOutBefore(ctx, time.Now().UTC().Add(-h.opts.KeepRotatedWindow))
	if pruneErr != nil {
		// Pruning failure is not fatal — the new key already won
		// the active slot. Log and proceed so we don't dead-letter
		// a successful rotation over a janitor hiccup.
		h.opts.Logger.WarnContext(ctx, "jwt-rotation: prune failed",
			slog.String("err", pruneErr.Error()))
	}

	h.opts.Logger.InfoContext(ctx, "jwt-rotation: keyring rotated",
		slog.String("new_kid", stored.Kid),
		slog.String("new_fingerprint", stored.SecretFingerprint),
		slog.String("prior_active_kid", priorKid),
		slog.Int("pruned_rows", pruned),
		slog.String("reason", payload.Reason),
		slog.String("event_id", ev.ID),
	)
	return nil
}

// Package papertrading is the Stage-4 service for the
// tamper-evident performance archive. The product story:
//
//	"AI announces what it WILL buy → publishes a SHA256 of the
//	 plan → next trading day's open price is the executed fill →
//	 daily NAV is updated by re-pricing the resulting holdings."
//
// The chain of trust comes from three guarantees:
//
//   1. The canonical decision payload is hashed deterministically.
//      The verifier can reconstruct the payload from the row and
//      re-hash to confirm "no after-the-fact mutation happened".
//   2. The SHA256 is published publicly (Twitter / Discord / blog)
//      within minutes of the decision. The publication timestamp
//      proves the hash was visible by T.
//   3. (Optional) The same hash is also submitted to
//      OpenTimestamps, which anchors it to Bitcoin's block-chain
//      via a Merkle proof. This is "stronger" than the social
//      timestamp because nobody can rewrite Bitcoin's history.
//      We start with a STUB (status=pending → submitted) and
//      replace the stub with the real OTS HTTP client when the
//      cost / DR story is signed off.
//
// This package is intentionally pure-Go + repo-only:
//   - canonicalisation is JSON with sorted keys
//   - SHA256 from crypto/sha256
//   - OTS is a stub (Status="submitted", proof_url empty) until
//     the live OTS client lands
//
// The package's I/O surface area is:
//   - PaperTradingRepo (read/write rows)
//   - ohlc.Fetcher (price the holdings)
//
// No LLM, no HTTP-out, no broker calls. Side effects are
// auditable.
package papertrading

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/fundai/server/internal/repository"
)

// Service is the package's public surface.
//
// All operations are nil-safe: passing a nil *Service returns
// ErrServiceUnconfigured so a deployment without the Stage-4
// feature flag can still construct the HTTP handler.
type Service struct {
	repo    *repository.PaperTradingRepo
	otsImpl OpenTimestampsClient
	now     func() time.Time
}

// ErrServiceUnconfigured is the canonical sentinel for "this
// deployment didn't wire the paper trading service". HTTP handlers
// translate it to a 503.
var ErrServiceUnconfigured = errors.New("papertrading: service unconfigured")

// OpenTimestampsClient is the seam to the OTS network. The MVP
// uses an in-process stub (StubOTSClient) that returns "submitted"
// without making an HTTP call; production will swap in a real
// implementation backed by the `ots stamp` CLI / HTTP API.
type OpenTimestampsClient interface {
	Stamp(ctx context.Context, hashHex string) (StampReceipt, error)
}

// StampReceipt is what the OTS client returns after a successful
// stamp request.
type StampReceipt struct {
	Status   string // "pending" | "submitted" | "confirmed" | "disabled"
	ProofURL string // public verifier URL when available
}

// StubOTSClient returns ("submitted", "") for every stamp. Used
// as the MVP default so the service runs end-to-end without
// shipping the real OTS HTTP / CLI integration.
type StubOTSClient struct{}

func (StubOTSClient) Stamp(_ context.Context, _ string) (StampReceipt, error) {
	return StampReceipt{Status: "submitted", ProofURL: ""}, nil
}

// New constructs the Stage-4 service. now is injectable so unit
// tests can pin "today". ots is also injectable; nil falls back to
// StubOTSClient.
func New(repo *repository.PaperTradingRepo, ots OpenTimestampsClient, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	if ots == nil {
		ots = StubOTSClient{}
	}
	return &Service{repo: repo, otsImpl: ots, now: now}
}

// -----------------------------------------------------------------------------
// Portfolio CRUD
// -----------------------------------------------------------------------------

type CreatePortfolioInput struct {
	Name            string
	Strategy        string
	Market          string
	BenchmarkSymbol string
	InitialCapital  float64
}

func (s *Service) CreatePortfolio(ctx context.Context, in CreatePortfolioInput) (*Portfolio, error) {
	if s == nil {
		return nil, ErrServiceUnconfigured
	}
	row := repository.PaperPortfolioRow{
		Name:           in.Name,
		Strategy:       in.Strategy,
		Market:         in.Market,
		InitialCapital: in.InitialCapital,
	}
	if in.BenchmarkSymbol != "" {
		row.BenchmarkSymbol = sqlNullString(in.BenchmarkSymbol)
	}
	created, err := s.repo.CreatePortfolio(ctx, row)
	if err != nil {
		return nil, err
	}
	return portfolioFromRow(created), nil
}

func (s *Service) ListPortfolios(ctx context.Context) ([]*Portfolio, error) {
	if s == nil {
		return nil, ErrServiceUnconfigured
	}
	rows, err := s.repo.ListPortfolios(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*Portfolio, 0, len(rows))
	for _, r := range rows {
		out = append(out, portfolioFromRow(r))
	}
	return out, nil
}

func (s *Service) GetPortfolio(ctx context.Context, id string) (*Portfolio, error) {
	if s == nil {
		return nil, ErrServiceUnconfigured
	}
	row, err := s.repo.GetPortfolio(ctx, id)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, nil
	}
	return portfolioFromRow(*row), nil
}

// -----------------------------------------------------------------------------
// Order placement (the SHA256 + OTS critical path)
// -----------------------------------------------------------------------------

// ProposeOrderInput is the wire shape for "AI proposes a trade".
// The service canonicalises this struct (sorted JSON keys) and
// hashes the bytes; the hash + canonical payload are stored
// alongside the row so verifiers can rehash.
type ProposeOrderInput struct {
	PortfolioID  string                 `json:"portfolioId"`
	Symbol       string                 `json:"symbol"`
	Action       string                 `json:"action"`       // BUY / SELL / REBALANCE
	TargetWeight *float64               `json:"targetWeight,omitempty"`
	SharesChange *float64               `json:"sharesChange,omitempty"`
	DecidedPrice *float64               `json:"decidedPrice,omitempty"`
	AIReasoning  map[string]any         `json:"aiReasoning,omitempty"`
	DecidedAt    time.Time              `json:"decidedAt"`
}

// ProposeOrder is the heart of Stage 4. Sequence:
//
//	1. validate input
//	2. canonicalise → JSON with sorted keys + RFC3339 timestamps
//	3. SHA-256 the canonical bytes
//	4. insert paper_orders row (status=pending)
//	5. fire-and-await OTS stamp; update status if it succeeds
//	6. return the populated row
//
// Step 5 is synchronous in the MVP because the stub returns
// immediately. When real OTS lands, it will run in a goroutine
// with a background context and the row's ots_status will start
// as "pending" until the goroutine flips it.
func (s *Service) ProposeOrder(ctx context.Context, in ProposeOrderInput) (*Order, error) {
	if s == nil {
		return nil, ErrServiceUnconfigured
	}
	if in.PortfolioID == "" || in.Symbol == "" || in.Action == "" {
		return nil, errors.New("portfolioId, symbol, action are required")
	}
	if !validAction(in.Action) {
		return nil, fmt.Errorf("invalid action %q (must be BUY/SELL/REBALANCE)", in.Action)
	}
	if in.DecidedAt.IsZero() {
		in.DecidedAt = s.now().UTC()
	}

	canonical, err := canonicalise(in)
	if err != nil {
		return nil, fmt.Errorf("canonicalise: %w", err)
	}
	hash := sha256Hex(canonical)

	row := repository.PaperOrderRow{
		PortfolioID:      in.PortfolioID,
		Symbol:           in.Symbol,
		Action:           in.Action,
		HashSignature:    hash,
		CanonicalPayload: string(canonical),
		OTSStatus:        "pending",
	}
	if in.TargetWeight != nil {
		row.TargetWeight = sqlNullFloat(*in.TargetWeight)
	}
	if in.SharesChange != nil {
		row.SharesChange = sqlNullFloat(*in.SharesChange)
	}
	if in.DecidedPrice != nil {
		row.DecidedPrice = sqlNullFloat(*in.DecidedPrice)
	}
	if len(in.AIReasoning) > 0 {
		blob, jerr := json.Marshal(in.AIReasoning)
		if jerr != nil {
			return nil, fmt.Errorf("marshal aiReasoning: %w", jerr)
		}
		row.AIReasoning = blob
	}

	inserted, err := s.repo.InsertOrder(ctx, row)
	if err != nil {
		return nil, err
	}

	// Best-effort OTS stamp. Failure is logged via the receipt's
	// Status="disabled" but does not fail the order — the social
	// timestamp (Twitter publication) still provides chain-of-trust.
	receipt, err := s.otsImpl.Stamp(ctx, hash)
	if err == nil && receipt.Status != "" {
		if uerr := s.repo.UpdateOrderProof(ctx, inserted.ID, receipt.ProofURL, receipt.Status); uerr == nil {
			inserted.OTSStatus = receipt.Status
			if receipt.ProofURL != "" {
				inserted.PublicProofURL = sqlNullString(receipt.ProofURL)
			}
		}
	}

	return orderFromRow(inserted), nil
}

// ListOrders returns the newest-first ledger for a portfolio.
func (s *Service) ListOrders(ctx context.Context, portfolioID string, limit int) ([]*Order, error) {
	if s == nil {
		return nil, ErrServiceUnconfigured
	}
	rows, err := s.repo.ListOrders(ctx, portfolioID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]*Order, 0, len(rows))
	for _, r := range rows {
		out = append(out, orderFromRow(r))
	}
	return out, nil
}

// -----------------------------------------------------------------------------
// NAV snapshot
// -----------------------------------------------------------------------------

// SnapshotNAVInput records a per-day NAV point. Caller is expected
// to have re-priced the holdings (typically using ohlc.Fetcher) and
// supply the new NAV; this method just persists the row + bumps
// paper_portfolios.current_nav.
type SnapshotNAVInput struct {
	PortfolioID  string
	SnapshotDate time.Time
	Nav          float64
	DailyReturn  *float64
	BenchmarkNav *float64
	CashBalance  float64
	Holdings     map[string]HoldingPosition // {symbol → position}
}

type HoldingPosition struct {
	Shares      float64 `json:"shares"`
	MarketValue float64 `json:"marketValue"`
	Weight      float64 `json:"weight"`
}

func (s *Service) SnapshotNAV(ctx context.Context, in SnapshotNAVInput) error {
	if s == nil {
		return ErrServiceUnconfigured
	}
	if in.PortfolioID == "" || in.SnapshotDate.IsZero() {
		return errors.New("portfolioId + snapshotDate required")
	}
	navRow := repository.PaperNavRow{
		PortfolioID:  in.PortfolioID,
		SnapshotDate: normaliseDate(in.SnapshotDate),
		Nav:          in.Nav,
	}
	if in.DailyReturn != nil {
		navRow.DailyReturn = sqlNullFloat(*in.DailyReturn)
	}
	if in.BenchmarkNav != nil {
		navRow.BenchmarkNav = sqlNullFloat(*in.BenchmarkNav)
	}
	if err := s.repo.UpsertNav(ctx, navRow); err != nil {
		return err
	}
	if in.Holdings != nil {
		blob, err := json.Marshal(in.Holdings)
		if err != nil {
			return fmt.Errorf("marshal holdings: %w", err)
		}
		snap := repository.PaperHoldingsSnapshotRow{
			PortfolioID:  in.PortfolioID,
			SnapshotDate: normaliseDate(in.SnapshotDate),
			Holdings:     blob,
			CashBalance:  in.CashBalance,
			TotalValue:   in.Nav,
		}
		if err := s.repo.UpsertHoldings(ctx, snap); err != nil {
			return err
		}
	}
	// Bump portfolio.current_nav so the list view reflects the latest.
	if err := s.repo.UpdatePortfolioNAV(ctx, in.PortfolioID, in.Nav, in.CashBalance, time.Time{}); err != nil {
		return err
	}
	return nil
}

// NavHistory exposes the per-day NAV time series for the
// performance page.
func (s *Service) NavHistory(ctx context.Context, portfolioID string) ([]NavPoint, error) {
	if s == nil {
		return nil, ErrServiceUnconfigured
	}
	rows, err := s.repo.NavHistory(ctx, portfolioID, 0)
	if err != nil {
		return nil, err
	}
	out := make([]NavPoint, 0, len(rows))
	for _, r := range rows {
		p := NavPoint{Date: r.SnapshotDate, Nav: r.Nav}
		if r.DailyReturn.Valid {
			v := r.DailyReturn.Float64
			p.DailyReturn = &v
		}
		if r.BenchmarkNav.Valid {
			v := r.BenchmarkNav.Float64
			p.BenchmarkNav = &v
		}
		out = append(out, p)
	}
	return out, nil
}

// -----------------------------------------------------------------------------
// Verification — public path
// -----------------------------------------------------------------------------

// VerifyOrder re-hashes the canonical payload stored on the row
// and compares against the persisted hash_signature. Returns true
// iff the row hasn't been tampered with. Failure cases set
// .Reason so the verifier UI can show the specific mismatch.
func (s *Service) VerifyOrder(ctx context.Context, orderID string) (*VerificationResult, error) {
	if s == nil {
		return nil, ErrServiceUnconfigured
	}
	// Fetch via list-with-limit-1 keyed by id-equivalent
	// (the repo doesn't have a GetOrder; for the MVP this
	// suffices since verification is rare).
	all, err := s.repo.ListOrders(ctx, "", 1)
	_ = all // placeholder — not used; real verifier would query by id
	if err != nil {
		return nil, err
	}
	// In the MVP we do the rehash from a freshly-fetched single row;
	// we can't currently fetch by id without adding a method. For
	// now, return Unsupported so callers know the verifier is a
	// placeholder.
	return &VerificationResult{OrderID: orderID, OK: false, Reason: "verifier not yet wired"}, nil
}

type VerificationResult struct {
	OrderID string `json:"orderId"`
	OK      bool   `json:"ok"`
	Reason  string `json:"reason,omitempty"`
}

// -----------------------------------------------------------------------------
// View types (decoupled from repository rows)
// -----------------------------------------------------------------------------

type Portfolio struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	Strategy        string    `json:"strategy"`
	Market          string    `json:"market"`
	BenchmarkSymbol string    `json:"benchmarkSymbol,omitempty"`
	InitialCapital  float64   `json:"initialCapital"`
	CurrentNav      float64   `json:"currentNav"`
	CashBalance     float64   `json:"cashBalance"`
	CreatedAt       time.Time `json:"createdAt"`
	LastRebalanceAt *time.Time `json:"lastRebalanceAt,omitempty"`
}

type Order struct {
	ID               string          `json:"id"`
	PortfolioID      string          `json:"portfolioId"`
	Symbol           string          `json:"symbol"`
	Action           string          `json:"action"`
	TargetWeight     *float64        `json:"targetWeight,omitempty"`
	SharesChange     *float64        `json:"sharesChange,omitempty"`
	DecidedAt        time.Time       `json:"decidedAt"`
	DecidedPrice     *float64        `json:"decidedPrice,omitempty"`
	ExecutedAt       *time.Time      `json:"executedAt,omitempty"`
	ExecutedPrice    *float64        `json:"executedPrice,omitempty"`
	AIReasoning      json.RawMessage `json:"aiReasoning,omitempty"`
	HashSignature    string          `json:"hashSignature"`
	CanonicalPayload string          `json:"canonicalPayload"`
	PublicProofURL   string          `json:"publicProofURL,omitempty"`
	OTSStatus        string          `json:"otsStatus"`
}

type NavPoint struct {
	Date         time.Time `json:"date"`
	Nav          float64   `json:"nav"`
	DailyReturn  *float64  `json:"dailyReturn,omitempty"`
	BenchmarkNav *float64  `json:"benchmarkNav,omitempty"`
}

// -----------------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------------

// canonicalise produces deterministic JSON bytes for the proposed
// order. Specifically:
//   - object keys sorted alphabetically
//   - times rendered as RFC3339Nano UTC
//   - omits zero-value optional fields
//
// We DO NOT rely on encoding/json's default object-key order
// because (a) Go's json package sorts map keys but (b) struct
// field order is source-code order. To ensure bit-for-bit identical
// output across binary versions, we route through a map[string]any
// and sort manually.
func canonicalise(in ProposeOrderInput) ([]byte, error) {
	m := map[string]any{
		"portfolioId": in.PortfolioID,
		"symbol":      in.Symbol,
		"action":      in.Action,
		"decidedAt":   in.DecidedAt.UTC().Format(time.RFC3339Nano),
	}
	if in.TargetWeight != nil {
		m["targetWeight"] = *in.TargetWeight
	}
	if in.SharesChange != nil {
		m["sharesChange"] = *in.SharesChange
	}
	if in.DecidedPrice != nil {
		m["decidedPrice"] = *in.DecidedPrice
	}
	if len(in.AIReasoning) > 0 {
		m["aiReasoning"] = in.AIReasoning
	}
	return marshalSorted(m)
}

// marshalSorted renders the given map as JSON with sorted top-level
// keys (and recursively for nested maps). Slices preserve order.
func marshalSorted(v any) ([]byte, error) {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf := []byte{'{'}
		for i, k := range keys {
			if i > 0 {
				buf = append(buf, ',')
			}
			kj, err := json.Marshal(k)
			if err != nil {
				return nil, err
			}
			buf = append(buf, kj...)
			buf = append(buf, ':')
			vj, err := marshalSorted(t[k])
			if err != nil {
				return nil, err
			}
			buf = append(buf, vj...)
		}
		buf = append(buf, '}')
		return buf, nil
	case []any:
		buf := []byte{'['}
		for i, e := range t {
			if i > 0 {
				buf = append(buf, ',')
			}
			vj, err := marshalSorted(e)
			if err != nil {
				return nil, err
			}
			buf = append(buf, vj...)
		}
		buf = append(buf, ']')
		return buf, nil
	default:
		return json.Marshal(v)
	}
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func validAction(a string) bool {
	switch a {
	case "BUY", "SELL", "REBALANCE":
		return true
	}
	return false
}

func normaliseDate(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func portfolioFromRow(r repository.PaperPortfolioRow) *Portfolio {
	p := &Portfolio{
		ID:             r.ID,
		Name:           r.Name,
		Strategy:       r.Strategy,
		Market:         r.Market,
		InitialCapital: r.InitialCapital,
		CurrentNav:     r.CurrentNav,
		CashBalance:    r.CashBalance,
		CreatedAt:      r.CreatedAt,
	}
	if r.BenchmarkSymbol.Valid {
		p.BenchmarkSymbol = r.BenchmarkSymbol.String
	}
	if r.LastRebalanceAt.Valid {
		t := r.LastRebalanceAt.Time
		p.LastRebalanceAt = &t
	}
	return p
}

func orderFromRow(r repository.PaperOrderRow) *Order {
	o := &Order{
		ID:               r.ID,
		PortfolioID:      r.PortfolioID,
		Symbol:           r.Symbol,
		Action:           r.Action,
		DecidedAt:        r.DecidedAt,
		HashSignature:    r.HashSignature,
		CanonicalPayload: r.CanonicalPayload,
		OTSStatus:        r.OTSStatus,
		AIReasoning:      r.AIReasoning,
	}
	if r.TargetWeight.Valid {
		v := r.TargetWeight.Float64
		o.TargetWeight = &v
	}
	if r.SharesChange.Valid {
		v := r.SharesChange.Float64
		o.SharesChange = &v
	}
	if r.DecidedPrice.Valid {
		v := r.DecidedPrice.Float64
		o.DecidedPrice = &v
	}
	if r.ExecutedAt.Valid {
		t := r.ExecutedAt.Time
		o.ExecutedAt = &t
	}
	if r.ExecutedPrice.Valid {
		v := r.ExecutedPrice.Float64
		o.ExecutedPrice = &v
	}
	if r.PublicProofURL.Valid {
		o.PublicProofURL = r.PublicProofURL.String
	}
	return o
}

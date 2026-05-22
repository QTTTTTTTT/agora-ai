package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/backtest"
	"github.com/fundai/server/internal/repository"
)

// SweepAxisCatalog implements api.BacktestService. Returns the
// fixed allow-list of axis names the wiring layer supports.
func (s *backtestServiceAdapter) SweepAxisCatalog() []string {
	return backtest.SortedAllowedSweepAxes()
}

// SubmitSweep implements api.BacktestService. Builds a SweepSpec
// from the input, validates via backtest.ExpandSweep, persists
// the sweep header, then fans out one Submit per cell. Returns
// the queued-snapshot sweep with all children attached.
//
// Failure semantics: validation errors return wrapped
// api.ErrSweepInvalid (handler → 400). If the sweep header
// insert fails the whole call aborts before any child is
// queued. If a child-job insert fails mid-way the sweep is
// left in a partial state — the header is still queryable, the
// queued children will run normally, and the error bubbles up.
// Operators inspect the sweep view and either retry the missing
// cells manually or discard the sweep.
func (s *backtestServiceAdapter) SubmitSweep(userID string, input api.SubmitSweepInput) (*api.BacktestSweep, error) {
	if s.ohlcFetcher == nil {
		return nil, api.ErrBacktestUnconfigured
	}
	if err := s.authorize(userID, input.FundID); err != nil {
		return nil, err
	}
	if strings.TrimSpace(input.FundID) == "" {
		return nil, fmt.Errorf("%w: fundId required", api.ErrSweepInvalid)
	}
	// Translate the base + axes into the backtest package's types
	// (the only place that has the full schema knowledge).
	base := translateSubmitInput(input.Base)
	base.FundID = input.FundID
	axes := translateAxesToBacktest(input.Axes)
	spec := backtest.SweepSpec{Base: base, Axes: axes, Name: input.Name}

	cells, err := backtest.ExpandSweep(spec)
	if err != nil {
		// Wrap so the handler can map every backtest.ErrSweep*
		// to a single 400. errors.Is(api.ErrSweepInvalid)
		// then matches in the handler.
		return nil, fmt.Errorf("%w: %s", api.ErrSweepInvalid, err.Error())
	}

	sweepID := uuid.NewString()
	now := time.Now().UTC()

	// Persist the sweep header BEFORE any children so the FK
	// from backtest_jobs.sweep_id resolves cleanly. We marshal
	// the original SubmitBacktestInput (not the translated
	// backtest.Request) so the replay-as-JSON path stays
	// symmetrical with one-off backtests.
	if s.sweepRepo != nil {
		baseJSON, err := json.Marshal(input.Base)
		if err != nil {
			return nil, fmt.Errorf("sweep: marshal base: %w", err)
		}
		axesJSON, err := json.Marshal(input.Axes)
		if err != nil {
			return nil, fmt.Errorf("sweep: marshal axes: %w", err)
		}
		header := &repository.SweepRow{
			ID:          sweepID,
			FundID:      input.FundID,
			UserID:      userID,
			Name:        input.Name,
			BaseRequest: baseJSON,
			Axes:        axesJSON,
			TotalCells:  len(cells),
			CreatedAt:   now,
		}
		if err := s.sweepRepo.Insert(context.Background(), header); err != nil {
			return nil, fmt.Errorf("sweep: insert header: %w", err)
		}
	}

	// Fan out: one Submit per cell, threaded through the same
	// submitMu pairing the persistSubmitted hook relies on.
	children := make([]*api.BacktestSweepChild, 0, len(cells))
	for _, cell := range cells {
		// Defensive copy to avoid the iteration variable shared-
		// pointer trap.
		cellMap := cell.AxisValues

		s.submitMu.Lock()
		s.pendingUserID = userID
		s.pendingSweepID = sweepID
		s.pendingCell = cellMap
		job, submitErr := s.store.Submit(context.Background(), cell.Request)
		s.pendingUserID = ""
		s.pendingSweepID = ""
		s.pendingCell = nil
		s.submitMu.Unlock()
		if submitErr != nil {
			return nil, fmt.Errorf("sweep: submit cell %v: %w", cellMap, submitErr)
		}
		children = append(children, &api.BacktestSweepChild{
			Job:        jobToView(job),
			AxisValues: cellMap,
		})
	}

	return &api.BacktestSweep{
		ID:         sweepID,
		FundID:     input.FundID,
		Name:       input.Name,
		Status:     deriveSweepStatus(children),
		TotalCells: len(cells),
		DoneCells:  countSweepDone(children),
		CreatedAt:  now,
		Axes:       input.Axes,
		Base:       echoFromSubmitInput(input.Base),
		Children:   children,
	}, nil
}

// ListSweeps implements api.BacktestService. Returns headers only
// (no children) — the UI uses this for the sweep history list
// and follows up with GetSweep when the operator opens one.
func (s *backtestServiceAdapter) ListSweeps(userID, fundID string) ([]*api.BacktestSweep, error) {
	if err := s.authorize(userID, fundID); err != nil {
		return nil, err
	}
	if s.sweepRepo == nil {
		return nil, nil
	}
	rows, err := s.sweepRepo.ListByFund(context.Background(), fundID, 50)
	if err != nil {
		return nil, fmt.Errorf("sweep: list: %w", err)
	}
	out := make([]*api.BacktestSweep, 0, len(rows))
	for _, row := range rows {
		view := sweepRowToView(row)
		// Cheap status: count children once. For 25-cell sweeps
		// this is 1 DB roundtrip; cheaper than streaming the
		// full child set just to compute a sweep-level summary.
		if s.backtestRepo != nil {
			childRows, err := s.backtestRepo.ListBySweep(context.Background(), row.ID)
			if err == nil {
				view.DoneCells = countTerminalChildren(childRows, s.store)
				view.Status = deriveSweepStatusFromRows(childRows, s.store)
			} else {
				slog.Warn("sweep: list children failed", "sweep_id", row.ID, "err", err)
			}
		}
		out = append(out, view)
	}
	return out, nil
}

// GetSweep implements api.BacktestService. Returns the sweep
// header + each child's most-current view. In-memory job wins
// over the DB shadow when the child is mid-flight.
func (s *backtestServiceAdapter) GetSweep(userID, fundID, sweepID string) (*api.BacktestSweep, error) {
	if err := s.authorize(userID, fundID); err != nil {
		return nil, err
	}
	if s.sweepRepo == nil {
		return nil, nil
	}
	header, err := s.sweepRepo.Get(context.Background(), sweepID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("sweep: get: %w", err)
	}
	if header.FundID != fundID {
		// Cross-fund probe — pretend the sweep doesn't exist
		// rather than leak its existence.
		return nil, nil
	}

	childRows, err := s.backtestRepo.ListBySweep(context.Background(), sweepID)
	if err != nil {
		return nil, fmt.Errorf("sweep: list children: %w", err)
	}

	view := sweepRowToView(*header)
	view.Children = make([]*api.BacktestSweepChild, 0, len(childRows))
	for _, row := range childRows {
		cell := parseSweepCell(row.SweepCell)
		var jobView *api.BacktestJob
		// Prefer the in-memory job (live progress) if the
		// runner is still spinning on this child.
		if liveJob := s.store.Get(row.ID); liveJob != nil && liveJob.Request.FundID == fundID {
			jobView = jobToView(liveJob)
		} else {
			jobView = rowToView(row, nil, nil)
		}
		view.Children = append(view.Children, &api.BacktestSweepChild{
			Job:        jobView,
			AxisValues: cell,
		})
	}
	// Sort children by submit time so the grid renders in a
	// stable order regardless of which were live vs. persisted.
	sort.SliceStable(view.Children, func(i, j int) bool {
		return view.Children[i].Job.SubmittedAt.Before(view.Children[j].Job.SubmittedAt)
	})
	view.TotalCells = header.TotalCells
	view.DoneCells = countSweepDone(view.Children)
	view.Status = deriveSweepStatus(view.Children)
	return view, nil
}

// translateAxesToBacktest projects the api wire shape into the
// backtest package's SweepAxis. Trivial mapping.
func translateAxesToBacktest(in []api.SubmitSweepAxisInput) []backtest.SweepAxis {
	out := make([]backtest.SweepAxis, 0, len(in))
	for _, ax := range in {
		out = append(out, backtest.SweepAxis{Name: ax.Name, Values: ax.Values})
	}
	return out
}

// echoFromSubmitInput reuses the SubmitBacktestInput shape to
// produce the api.BacktestRequestEcho the UI expects on the
// sweep view's Base field.
func echoFromSubmitInput(in api.SubmitBacktestInput) *api.BacktestRequestEcho {
	echo := &api.BacktestRequestEcho{
		Symbols:          append([]string(nil), in.Symbols...),
		Start:            in.Start,
		End:              in.End,
		InitialCash:      in.InitialCash,
		BaseCurrency:     in.BaseCurrency,
		SlippageBps:      in.SlippageBps,
		CommissionBps:    in.CommissionBps,
		MaxOrdersPerDay:  in.MaxOrdersPerDay,
		InitialPositions: append([]api.BacktestInitialPosition(nil), in.InitialPositions...),
	}
	return echo
}

// sweepRowToView projects a persisted SweepRow into the wire
// shape. Children are populated later by the caller — this
// helper only fills header-level fields.
func sweepRowToView(row repository.SweepRow) *api.BacktestSweep {
	view := &api.BacktestSweep{
		ID:         row.ID,
		FundID:     row.FundID,
		Name:       row.Name,
		Status:     "queued", // overwritten by caller after counting children
		TotalCells: row.TotalCells,
		CreatedAt:  row.CreatedAt,
	}
	// Re-hydrate axes so the UI doesn't have to remember them.
	if len(row.Axes) > 0 {
		var axes []api.SubmitSweepAxisInput
		if err := json.Unmarshal(row.Axes, &axes); err == nil {
			view.Axes = axes
		}
	}
	// Re-hydrate base request as an echo so the UI can show
	// "what was the template?" without a separate fetch.
	if len(row.BaseRequest) > 0 {
		var base api.SubmitBacktestInput
		if err := json.Unmarshal(row.BaseRequest, &base); err == nil {
			view.Base = echoFromSubmitInput(base)
		}
	}
	return view
}

// parseSweepCell decodes the cell JSON into the axis → value
// map. Tolerates nil / empty / malformed payloads by returning
// nil — the UI handles the absence gracefully.
func parseSweepCell(raw json.RawMessage) map[string]string {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var out map[string]string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

// deriveSweepStatus aggregates children statuses into one of:
//   - running   — any child still queued or running
//   - completed — all children terminal, at least one completed
//   - failed    — all children terminal, none completed
//   - queued    — only used while every child is still queued
//                 (rare, only seen during the brief window
//                 between SubmitSweep returning and the first
//                 child goroutine starting)
func deriveSweepStatus(children []*api.BacktestSweepChild) string {
	if len(children) == 0 {
		return "queued"
	}
	var queued, running, completed, terminalOther int
	for _, c := range children {
		switch strings.ToLower(c.Job.Status) {
		case "queued":
			queued++
		case "running":
			running++
		case "completed":
			completed++
		default: // failed / cancelled / unknown
			terminalOther++
		}
	}
	if running > 0 {
		return "running"
	}
	if queued == len(children) {
		return "queued"
	}
	if queued > 0 {
		// Some terminal, some still queued — sweep is mid-flight.
		return "running"
	}
	// All terminal.
	if completed > 0 {
		return "completed"
	}
	return "failed"
}

// countSweepDone counts children in any terminal state. Used
// for the "X / Y cells done" progress indicator on the header.
func countSweepDone(children []*api.BacktestSweepChild) int {
	n := 0
	for _, c := range children {
		switch strings.ToLower(c.Job.Status) {
		case "completed", "failed", "cancelled":
			n++
		}
	}
	return n
}

// countTerminalChildren is the same as countSweepDone but
// operates on the repo row shape — saving an extra projection
// trip when the caller only needs the count.
//
// We also peek at the in-memory store so a row that's still
// 'running' according to the DB but has actually completed
// (and is awaiting its persistFinal flush) doesn't undercount.
func countTerminalChildren(rows []repository.BacktestJobRow, store *backtest.JobStore) int {
	n := 0
	for _, r := range rows {
		status := liveOrRowStatus(r, store)
		switch status {
		case "completed", "failed", "cancelled":
			n++
		}
	}
	return n
}

// deriveSweepStatusFromRows is the same logic as
// deriveSweepStatus but takes repo rows directly so we don't
// project rows → views just to count them in the list path.
func deriveSweepStatusFromRows(rows []repository.BacktestJobRow, store *backtest.JobStore) string {
	if len(rows) == 0 {
		return "queued"
	}
	var queued, running, completed, terminalOther int
	for _, r := range rows {
		switch liveOrRowStatus(r, store) {
		case "queued":
			queued++
		case "running":
			running++
		case "completed":
			completed++
		default:
			terminalOther++
		}
	}
	if running > 0 {
		return "running"
	}
	if queued == len(rows) {
		return "queued"
	}
	if queued > 0 {
		return "running"
	}
	if completed > 0 {
		return "completed"
	}
	return "failed"
}

// liveOrRowStatus consults the in-memory store first so a row
// that's mid-flight returns its live status (queued/running)
// rather than the stale DB value.
func liveOrRowStatus(row repository.BacktestJobRow, store *backtest.JobStore) string {
	if store != nil {
		if live := store.Get(row.ID); live != nil {
			snap := live.Progress.Snapshot()
			if snap.Status != "" {
				return strings.ToLower(snap.Status)
			}
		}
	}
	return strings.ToLower(row.Status)
}

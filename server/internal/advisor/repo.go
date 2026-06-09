// repo.go — persistence façade for the four advisor_* tables.
//
// Pattern mirrors internal/analystreport.Repo:
//
//   * One *sql.DB owned at construction.
//   * Strict validation of inputs at the door (we never let an
//     invalid PersonaPreset / Consultation / MasterReport reach
//     the DB).
//   * Single transaction per write (one Consultation + N child
//     reports atomically).
//
// Phase 0 wires only:
//   * GetPreset / ListPresets — needed by the preset picker even
//     before consultations exist.
//   * Verify the four tables exist on first call (cheap belt-and-
//     braces against running against a DB where migration 098
//     was rolled back).
//
// Phase 1 adds SaveConsultation + ListConsultations + GetConsultation
// once the agent layer is ready to produce reports.

package advisor

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"
)

// ErrNotFound matches the sentinel used by other repos in this
// codebase (analystreport.ErrNotFound, etc.) so handlers can use a
// single errors.Is check.
var ErrNotFound = errors.New("advisor: not found")

// Repo is the persistence façade. Construct with NewRepo(db).
// nil-safe: every read method returns an empty slice + nil error
// when r.db == nil, so callers that haven't wired the DB (tests,
// degraded boot) don't crash.
type Repo struct {
	db *sql.DB
}

// NewRepo wires the repo. Passing nil yields a no-op repo (reads
// return empty, writes return an "advisor: repo not initialised"
// error).
func NewRepo(db *sql.DB) *Repo { return &Repo{db: db} }

// --- Preset reads ------------------------------------------------------------

// Get returns a single preset by key. Returns ErrPresetNotFound
// for unknown keys or disabled rows (handlers map both to 404).
func (r *Repo) Get(ctx context.Context, key string) (PersonaPreset, error) {
	if r == nil || r.db == nil {
		return PersonaPreset{}, ErrPresetNotFound
	}
	const q = `
		SELECT preset_key, label_zh, label_en,
		       COALESCE(description_zh, ''), COALESCE(description_en, ''),
		       COALESCE(master_keys, '{}'::TEXT[]),
		       COALESCE(tactic_keys, '{}'::TEXT[]),
		       enabled, sort_order
		  FROM advisor_persona_presets
		 WHERE preset_key = $1`
	var (
		p              PersonaPreset
		masterKeysArr  pq.StringArray
		tacticKeysArr  pq.StringArray
	)
	err := r.db.QueryRowContext(ctx, q, NormalizePresetKey(key)).Scan(
		&p.Key, &p.LabelZh, &p.LabelEn,
		&p.DescriptionZh, &p.DescriptionEn,
		&masterKeysArr, &tacticKeysArr,
		&p.Enabled, &p.SortOrder,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return PersonaPreset{}, ErrPresetNotFound
	}
	if err != nil {
		return PersonaPreset{}, fmt.Errorf("advisor: get preset: %w", err)
	}
	if !p.Enabled {
		// We intentionally fold disabled into not-found so the
		// handler doesn't need to know about the column. Admins
		// re-enable from the seed UI; users never see the row.
		return PersonaPreset{}, ErrPresetNotFound
	}
	p.MasterKeys = dedupAndTrim([]string(masterKeysArr))
	p.TacticKeys = dedupAndTrim([]string(tacticKeysArr))
	return p, nil
}

// List returns every preset, ordered by sort_order ASC then key.
// When enabledOnly is true, hides disabled rows.
func (r *Repo) List(ctx context.Context, enabledOnly bool) ([]PersonaPreset, error) {
	if r == nil || r.db == nil {
		return nil, nil
	}
	q := `
		SELECT preset_key, label_zh, label_en,
		       COALESCE(description_zh, ''), COALESCE(description_en, ''),
		       COALESCE(master_keys, '{}'::TEXT[]),
		       COALESCE(tactic_keys, '{}'::TEXT[]),
		       enabled, sort_order
		  FROM advisor_persona_presets`
	if enabledOnly {
		q += " WHERE enabled = TRUE"
	}
	q += " ORDER BY sort_order ASC, preset_key ASC"

	rows, err := r.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("advisor: list presets: %w", err)
	}
	defer rows.Close()

	var out []PersonaPreset
	for rows.Next() {
		var (
			p             PersonaPreset
			masterKeysArr pq.StringArray
			tacticKeysArr pq.StringArray
		)
		if err := rows.Scan(
			&p.Key, &p.LabelZh, &p.LabelEn,
			&p.DescriptionZh, &p.DescriptionEn,
			&masterKeysArr, &tacticKeysArr,
			&p.Enabled, &p.SortOrder,
		); err != nil {
			return nil, fmt.Errorf("advisor: scan preset: %w", err)
		}
		p.MasterKeys = dedupAndTrim([]string(masterKeysArr))
		p.TacticKeys = dedupAndTrim([]string(tacticKeysArr))
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("advisor: iterate presets: %w", err)
	}
	return out, nil
}

// --- Consultation writes ----------------------------------------------------

// SavedConsultation is the row identifier returned by SaveConsultation so
// callers can immediately render a "consultation #abc-123" URL without a
// second round-trip.
type SavedConsultation struct {
	ID        string
	CreatedAt time.Time
}

// SaveConsultationInput bundles the parent + child rows into a single
// transactional write. MasterReports and TacticReports are appended into
// the per-type child tables; either (or both) can be empty.
type SaveConsultationInput struct {
	UserID              string
	Symbol              string
	// SymbolName is the issuer's short Chinese / English name
	// (e.g. "德科立"). Nullable in the DB; pass empty if the
	// caller doesn't know it. We never overwrite this with an
	// empty string on the read path — historical rows can have
	// NULL and that's fine.
	SymbolName          string
	Market              string
	AssetClass          string
	PresetKey           string
	AggregateVerdict    string
	AggregateConfidence int
	ConsensusScore      float64
	Notes               string
	PriceAtConsult      *float64
	MasterReports       []MasterReportRow
	TacticReports       []TacticReportRow
}

// SaveConsultation persists one consultation + its child master and
// tactic reports atomically. Returns the newly-allocated row id.
func (r *Repo) SaveConsultation(ctx context.Context, in SaveConsultationInput) (SavedConsultation, error) {
	if r == nil || r.db == nil {
		return SavedConsultation{}, errors.New("advisor: repo not initialised")
	}
	if strings.TrimSpace(in.UserID) == "" {
		return SavedConsultation{}, errors.New("advisor: UserID required")
	}
	if strings.TrimSpace(in.Symbol) == "" {
		return SavedConsultation{}, errors.New("advisor: Symbol required")
	}
	if strings.TrimSpace(in.PresetKey) == "" {
		return SavedConsultation{}, errors.New("advisor: PresetKey required")
	}
	verdict := strings.ToUpper(strings.TrimSpace(in.AggregateVerdict))
	if verdict == "" {
		verdict = "HOLD"
	}
	if in.AggregateConfidence < 0 {
		in.AggregateConfidence = 0
	}
	if in.AggregateConfidence > 100 {
		in.AggregateConfidence = 100
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return SavedConsultation{}, fmt.Errorf("advisor: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	const consultSQL = `
		INSERT INTO advisor_consultations
		    (user_id, symbol, symbol_name, market, asset_class, preset_key,
		     aggregate_verdict, aggregate_confidence, consensus_score,
		     notes, price_at_consult)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING id, created_at`
	var (
		consultID string
		createdAt time.Time
	)
	// symbol_name is nullable so the column accepts NULL for the
	// many older rows + the in-memory mock loaders that don't
	// resolve a name. Pass sql.NullString explicitly so the empty
	// case lands as SQL NULL rather than an empty TEXT — keeps
	// downstream queries that do COALESCE / IS NULL semantics
	// consistent.
	var symbolName sql.NullString
	if s := strings.TrimSpace(in.SymbolName); s != "" {
		symbolName = sql.NullString{String: s, Valid: true}
	}
	if err := tx.QueryRowContext(ctx, consultSQL,
		in.UserID,
		strings.ToUpper(strings.TrimSpace(in.Symbol)),
		symbolName,
		strings.ToLower(strings.TrimSpace(in.Market)),
		strings.ToLower(strings.TrimSpace(in.AssetClass)),
		NormalizePresetKey(in.PresetKey),
		verdict,
		in.AggregateConfidence,
		in.ConsensusScore,
		in.Notes,
		in.PriceAtConsult,
	).Scan(&consultID, &createdAt); err != nil {
		return SavedConsultation{}, fmt.Errorf("advisor: insert consultation: %w", err)
	}

	if len(in.MasterReports) > 0 {
		const masterSQL = `
			INSERT INTO advisor_master_reports
			    (consultation_id, master_key, master_name_zh, master_name_en,
			     verdict, confidence, thesis,
			     key_reasons, key_risks, master_specific, red_lines_hit,
			     llm_model, prompt_tokens, completion_tokens)
			VALUES ($1, $2, $3, $4, $5, $6, $7,
			        $8::jsonb, $9::jsonb, $10::jsonb, $11::jsonb,
			        $12, $13, $14)`
		stmt, err := tx.PrepareContext(ctx, masterSQL)
		if err != nil {
			return SavedConsultation{}, fmt.Errorf("advisor: prepare master insert: %w", err)
		}
		for _, m := range in.MasterReports {
			reasonsJSON, _ := json.Marshal(stringSliceOrEmpty(m.KeyReasons))
			risksJSON, _ := json.Marshal(stringSliceOrEmpty(m.KeyRisks))
			specificJSON, _ := json.Marshal(mapOrEmpty(m.MasterSpecific))
			redLinesJSON, _ := json.Marshal(stringSliceOrEmpty(m.RedLinesHit))
			if _, err := stmt.ExecContext(ctx,
				consultID,
				strings.ToLower(strings.TrimSpace(m.MasterKey)),
				m.MasterNameZh,
				m.MasterNameEn,
				strings.ToUpper(strings.TrimSpace(m.Verdict)),
				m.Confidence,
				m.Thesis,
				string(reasonsJSON),
				string(risksJSON),
				string(specificJSON),
				string(redLinesJSON),
				m.LLMModel,
				0, // prompt_tokens — populated when caller supplies usage metadata
				0,
			); err != nil {
				_ = stmt.Close()
				return SavedConsultation{}, fmt.Errorf("advisor: insert master %q: %w", m.MasterKey, err)
			}
		}
		_ = stmt.Close()
	}

	if len(in.TacticReports) > 0 {
		const tacticSQL = `
			INSERT INTO advisor_tactic_reports
			    (consultation_id, tactic_key, tactic_name_zh, tactic_name_en,
			     verdict, confidence, thesis,
			     entry_price_low, entry_price_high, stop_loss_price,
			     target_t1, target_t3, expected_holding_days,
			     score, key_reasons, key_risks, red_lines_hit,
			     market_regime_pass, market_regime_reason)
			VALUES ($1, $2, $3, $4, $5, $6, $7,
			        $8, $9, $10, $11, $12, $13, $14,
			        $15::jsonb, $16::jsonb, $17::jsonb,
			        $18, $19)`
		stmt, err := tx.PrepareContext(ctx, tacticSQL)
		if err != nil {
			return SavedConsultation{}, fmt.Errorf("advisor: prepare tactic insert: %w", err)
		}
		for _, t := range in.TacticReports {
			reasonsJSON, _ := json.Marshal(stringSliceOrEmpty(t.KeyReasons))
			risksJSON, _ := json.Marshal(stringSliceOrEmpty(t.KeyRisks))
			redLinesJSON, _ := json.Marshal(stringSliceOrEmpty(t.RedLinesHit))
			if _, err := stmt.ExecContext(ctx,
				consultID,
				strings.ToLower(strings.TrimSpace(t.TacticKey)),
				t.TacticNameZh,
				t.TacticNameEn,
				strings.ToUpper(strings.TrimSpace(t.Verdict)),
				t.Confidence,
				t.Thesis,
				t.EntryPriceLow,
				t.EntryPriceHigh,
				t.StopLossPrice,
				t.TargetT1,
				t.TargetT3,
				t.ExpectedHoldingDays,
				t.Score,
				string(reasonsJSON),
				string(risksJSON),
				string(redLinesJSON),
				t.MarketRegimePass,
				t.MarketRegimeReason,
			); err != nil {
				_ = stmt.Close()
				return SavedConsultation{}, fmt.Errorf("advisor: insert tactic %q: %w", t.TacticKey, err)
			}
		}
		_ = stmt.Close()
	}

	if err := tx.Commit(); err != nil {
		return SavedConsultation{}, fmt.Errorf("advisor: commit: %w", err)
	}
	return SavedConsultation{ID: consultID, CreatedAt: createdAt}, nil
}

// --- Consultation reads -----------------------------------------------------

// ConsultationRow is the read shape for advisor_consultations + its
// joined children. CreatedAt is the parent row's timestamp; the child
// rows expose their own GeneratedAt for fine-grained ordering.
type ConsultationRow struct {
	ID                  string
	UserID              string
	Symbol              string
	// SymbolName mirrors SaveConsultationInput.SymbolName. Empty
	// for legacy rows persisted before migration 105 added the
	// column — callers must tolerate the empty case.
	SymbolName          string
	Market              string
	AssetClass          string
	PresetKey           string
	AggregateVerdict    string
	AggregateConfidence int
	ConsensusScore      float64
	Notes               string
	PriceAtConsult      *float64
	CreatedAt           time.Time
	MasterReports       []MasterReportRow
	TacticReports       []TacticReportRow
}

// ListConsultationsParams filters list reads. Always scoped to a
// single user so a future "anonymous browse" mode has to add a
// separate read method rather than accidentally leaking history.
type ListConsultationsParams struct {
	UserID          string
	Symbol          string
	PresetKey       string
	Limit           int
	IncludeChildren bool
}

// ListConsultations returns the most recent consultations for a
// user, ordered by created_at DESC. Caps at 200 to keep a single
// query bounded.
func (r *Repo) ListConsultations(ctx context.Context, p ListConsultationsParams) ([]ConsultationRow, error) {
	if r == nil || r.db == nil {
		return nil, nil
	}
	if strings.TrimSpace(p.UserID) == "" {
		return nil, errors.New("advisor: ListConsultations requires UserID")
	}
	conds := []string{"user_id = $1"}
	args := []any{p.UserID}
	if s := strings.ToUpper(strings.TrimSpace(p.Symbol)); s != "" {
		args = append(args, s)
		conds = append(conds, fmt.Sprintf("symbol = $%d", len(args)))
	}
	if s := NormalizePresetKey(p.PresetKey); s != "" {
		args = append(args, s)
		conds = append(conds, fmt.Sprintf("preset_key = $%d", len(args)))
	}
	limit := p.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	args = append(args, limit)

	q := fmt.Sprintf(`
		SELECT id, user_id, symbol, symbol_name, market, asset_class, preset_key,
		       aggregate_verdict, aggregate_confidence, consensus_score,
		       notes, price_at_consult, created_at
		  FROM advisor_consultations
		 WHERE %s
	  ORDER BY created_at DESC
	     LIMIT $%d`, strings.Join(conds, " AND "), len(args))

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("advisor: list consultations: %w", err)
	}
	defer rows.Close()

	var out []ConsultationRow
	for rows.Next() {
		var (
			row        ConsultationRow
			symbolName sql.NullString
			price      sql.NullFloat64
		)
		if err := rows.Scan(
			&row.ID, &row.UserID, &row.Symbol, &symbolName, &row.Market, &row.AssetClass, &row.PresetKey,
			&row.AggregateVerdict, &row.AggregateConfidence, &row.ConsensusScore,
			&row.Notes, &price, &row.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("advisor: scan consultation: %w", err)
		}
		if symbolName.Valid {
			row.SymbolName = symbolName.String
		}
		if price.Valid {
			v := price.Float64
			row.PriceAtConsult = &v
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("advisor: iter consultations: %w", err)
	}

	if p.IncludeChildren && len(out) > 0 {
		ids := make([]string, len(out))
		for i := range out {
			ids[i] = out[i].ID
		}
		masters, err := r.fetchMasterReportsForConsultations(ctx, ids)
		if err != nil {
			return nil, err
		}
		tactics, err := r.fetchTacticReportsForConsultations(ctx, ids)
		if err != nil {
			return nil, err
		}
		for i := range out {
			out[i].MasterReports = masters[out[i].ID]
			out[i].TacticReports = tactics[out[i].ID]
		}
	}
	return out, nil
}

// GetConsultation returns one consultation + every child report.
// Returns ErrNotFound when the row doesn't exist or isn't owned by
// `userID` (we never leak rows across users).
func (r *Repo) GetConsultation(ctx context.Context, userID, id string) (ConsultationRow, error) {
	if r == nil || r.db == nil {
		return ConsultationRow{}, ErrNotFound
	}
	const q = `
		SELECT id, user_id, symbol, symbol_name, market, asset_class, preset_key,
		       aggregate_verdict, aggregate_confidence, consensus_score,
		       notes, price_at_consult, created_at
		  FROM advisor_consultations
		 WHERE id = $1 AND user_id = $2`
	var (
		row        ConsultationRow
		symbolName sql.NullString
		price      sql.NullFloat64
	)
	err := r.db.QueryRowContext(ctx, q, id, userID).Scan(
		&row.ID, &row.UserID, &row.Symbol, &symbolName, &row.Market, &row.AssetClass, &row.PresetKey,
		&row.AggregateVerdict, &row.AggregateConfidence, &row.ConsensusScore,
		&row.Notes, &price, &row.CreatedAt,
	)
	if symbolName.Valid {
		row.SymbolName = symbolName.String
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ConsultationRow{}, ErrNotFound
	}
	if err != nil {
		return ConsultationRow{}, fmt.Errorf("advisor: get consultation: %w", err)
	}
	if price.Valid {
		v := price.Float64
		row.PriceAtConsult = &v
	}
	masters, err := r.fetchMasterReportsForConsultations(ctx, []string{row.ID})
	if err != nil {
		return ConsultationRow{}, err
	}
	tactics, err := r.fetchTacticReportsForConsultations(ctx, []string{row.ID})
	if err != nil {
		return ConsultationRow{}, err
	}
	row.MasterReports = masters[row.ID]
	row.TacticReports = tactics[row.ID]
	return row, nil
}

func (r *Repo) fetchMasterReportsForConsultations(ctx context.Context, ids []string) (map[string][]MasterReportRow, error) {
	out := make(map[string][]MasterReportRow, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	const q = `
		SELECT consultation_id, master_key, master_name_zh, master_name_en,
		       verdict, confidence, thesis,
		       key_reasons, key_risks, master_specific, red_lines_hit,
		       llm_model, generated_at
		  FROM advisor_master_reports
		 WHERE consultation_id = ANY($1)
	  ORDER BY master_key ASC`
	rows, err := r.db.QueryContext(ctx, q, pq.Array(ids))
	if err != nil {
		return nil, fmt.Errorf("advisor: fetch master reports: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			parentID string
			row      MasterReportRow
			reasonsJSON, risksJSON, specificJSON, redLinesJSON []byte
		)
		if err := rows.Scan(
			&parentID, &row.MasterKey, &row.MasterNameZh, &row.MasterNameEn,
			&row.Verdict, &row.Confidence, &row.Thesis,
			&reasonsJSON, &risksJSON, &specificJSON, &redLinesJSON,
			&row.LLMModel, &row.GeneratedAt,
		); err != nil {
			return nil, fmt.Errorf("advisor: scan master report: %w", err)
		}
		_ = json.Unmarshal(reasonsJSON, &row.KeyReasons)
		_ = json.Unmarshal(risksJSON, &row.KeyRisks)
		row.MasterSpecific = map[string]any{}
		_ = json.Unmarshal(specificJSON, &row.MasterSpecific)
		_ = json.Unmarshal(redLinesJSON, &row.RedLinesHit)
		out[parentID] = append(out[parentID], row)
	}
	return out, rows.Err()
}

func (r *Repo) fetchTacticReportsForConsultations(ctx context.Context, ids []string) (map[string][]TacticReportRow, error) {
	out := make(map[string][]TacticReportRow, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	const q = `
		SELECT consultation_id, tactic_key, tactic_name_zh, tactic_name_en,
		       verdict, confidence, thesis,
		       entry_price_low, entry_price_high, stop_loss_price,
		       target_t1, target_t3, expected_holding_days, score,
		       key_reasons, key_risks, red_lines_hit,
		       market_regime_pass, market_regime_reason, generated_at
		  FROM advisor_tactic_reports
		 WHERE consultation_id = ANY($1)
	  ORDER BY tactic_key ASC`
	rows, err := r.db.QueryContext(ctx, q, pq.Array(ids))
	if err != nil {
		return nil, fmt.Errorf("advisor: fetch tactic reports: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			parentID                                                              string
			row                                                                   TacticReportRow
			entryLow, entryHigh, stop, t1, t3                                     sql.NullFloat64
			holdingDays                                                           sql.NullInt64
			reasonsJSON, risksJSON, redLinesJSON                                  []byte
		)
		if err := rows.Scan(
			&parentID, &row.TacticKey, &row.TacticNameZh, &row.TacticNameEn,
			&row.Verdict, &row.Confidence, &row.Thesis,
			&entryLow, &entryHigh, &stop, &t1, &t3, &holdingDays, &row.Score,
			&reasonsJSON, &risksJSON, &redLinesJSON,
			&row.MarketRegimePass, &row.MarketRegimeReason, &row.GeneratedAt,
		); err != nil {
			return nil, fmt.Errorf("advisor: scan tactic report: %w", err)
		}
		if entryLow.Valid {
			v := entryLow.Float64
			row.EntryPriceLow = &v
		}
		if entryHigh.Valid {
			v := entryHigh.Float64
			row.EntryPriceHigh = &v
		}
		if stop.Valid {
			v := stop.Float64
			row.StopLossPrice = &v
		}
		if t1.Valid {
			v := t1.Float64
			row.TargetT1 = &v
		}
		if t3.Valid {
			v := t3.Float64
			row.TargetT3 = &v
		}
		if holdingDays.Valid {
			v := int(holdingDays.Int64)
			row.ExpectedHoldingDays = &v
		}
		_ = json.Unmarshal(reasonsJSON, &row.KeyReasons)
		_ = json.Unmarshal(risksJSON, &row.KeyRisks)
		_ = json.Unmarshal(redLinesJSON, &row.RedLinesHit)
		out[parentID] = append(out[parentID], row)
	}
	return out, rows.Err()
}

// ReputationConsultation is the projection the
// advisor_reputation_loop needs: parent id + symbol + asof + every
// child report (master and tactic) flattened into the per-agent
// rows the loop will turn into agent_reputation_outcomes.
type ReputationConsultation struct {
	ID             string
	Symbol         string
	Market         string
	AssetClass     string
	CreatedAt      time.Time
	PriceAtConsult *float64
	MasterReports  []MasterReportRow
	TacticReports  []TacticReportRow
}

// ListConsultationsForReputation scans for consultations whose
// created_at falls inside [olderThan-window, olderThan] — i.e.
// consultations old enough for the horizon's forward window to
// have closed but not too old to re-grade infinitely. Across
// users, since reputation is global, not per-user. Hard-caps at
// `limit` to keep one loop iteration bounded.
func (r *Repo) ListConsultationsForReputation(
	ctx context.Context,
	olderThan time.Time,
	windowDays int,
	limit int,
) ([]ReputationConsultation, error) {
	if r == nil || r.db == nil {
		return nil, nil
	}
	if windowDays <= 0 {
		windowDays = 30
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	since := olderThan.AddDate(0, 0, -windowDays)

	const q = `
		SELECT id, symbol, market, asset_class, price_at_consult, created_at
		  FROM advisor_consultations
		 WHERE created_at <= $1
		   AND created_at >= $2
	  ORDER BY created_at ASC
	     LIMIT $3`
	rows, err := r.db.QueryContext(ctx, q, olderThan, since, limit)
	if err != nil {
		return nil, fmt.Errorf("advisor: list consultations for reputation: %w", err)
	}
	defer rows.Close()
	var out []ReputationConsultation
	for rows.Next() {
		var (
			rc    ReputationConsultation
			price sql.NullFloat64
		)
		if err := rows.Scan(&rc.ID, &rc.Symbol, &rc.Market, &rc.AssetClass, &price, &rc.CreatedAt); err != nil {
			return nil, fmt.Errorf("advisor: scan reputation consultation: %w", err)
		}
		if price.Valid {
			v := price.Float64
			rc.PriceAtConsult = &v
		}
		out = append(out, rc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("advisor: iter reputation consultations: %w", err)
	}
	if len(out) == 0 {
		return out, nil
	}
	ids := make([]string, len(out))
	for i := range out {
		ids[i] = out[i].ID
	}
	masters, err := r.fetchMasterReportsForConsultations(ctx, ids)
	if err != nil {
		return nil, err
	}
	tactics, err := r.fetchTacticReportsForConsultations(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].MasterReports = masters[out[i].ID]
		out[i].TacticReports = tactics[out[i].ID]
	}
	return out, nil
}

// BillingCallRow is the projection ByokCallLogPanel renders. We
// don't return the verdict bodies / reports here — the call log is
// strictly a "where did my money go" table, not a thesis viewer.
// One row per /consult call, plus the model the masters dispatched
// to so the user can confirm BYOK was honoured ("oh it really did
// hit my OpenAI key, not the platform pool").
type BillingCallRow struct {
	ID                string
	Symbol            string
	PresetKey         string
	AggregateVerdict  string
	ServiceUnitSource string
	ServiceUnitCost   int
	ModelsUsed        []string
	BYOKUsed          bool
	CreatedAt         time.Time
}

// ListBillingCallsParams gates the call-log read.
type ListBillingCallsParams struct {
	UserID   string
	Limit    int
	BYOKOnly bool
}

// ListBillingCalls returns the per-consult billing trail for the
// /settings/byok call-log panel. Sorted newest first, hard-capped
// at 200 rows. Skips rows where every report has empty model
// strings (the panel would render empty cells anyway).
func (r *Repo) ListBillingCalls(ctx context.Context, p ListBillingCallsParams) ([]BillingCallRow, error) {
	if r == nil || r.db == nil {
		return nil, nil
	}
	if strings.TrimSpace(p.UserID) == "" {
		return nil, errors.New("advisor: ListBillingCalls requires UserID")
	}
	limit := p.Limit
	if limit <= 0 || limit > 200 {
		limit = 30
	}
	// Use a LATERAL join so models_used is computed per-row
	// without a second round-trip. Mode-array via DISTINCT to
	// dedupe across N masters that all hit the same model.
	q := `
		SELECT c.id, c.symbol, c.preset_key, c.aggregate_verdict,
		       COALESCE(c.service_unit_source, ''),
		       COALESCE(c.service_unit_cost, 0),
		       c.created_at,
		       COALESCE(m.models, '{}'::text[])
		  FROM advisor_consultations c
		  LEFT JOIN LATERAL (
		      SELECT array_agg(DISTINCT llm_model) AS models
		        FROM advisor_master_reports
		       WHERE consultation_id = c.id
		         AND llm_model <> ''
		  ) m ON TRUE
		 WHERE c.user_id = $1
	  ORDER BY c.created_at DESC
	     LIMIT $2`
	rows, err := r.db.QueryContext(ctx, q, p.UserID, limit)
	if err != nil {
		return nil, fmt.Errorf("advisor: list billing calls: %w", err)
	}
	defer rows.Close()
	var out []BillingCallRow
	for rows.Next() {
		var (
			row    BillingCallRow
			models pqStringArray
		)
		if err := rows.Scan(
			&row.ID, &row.Symbol, &row.PresetKey, &row.AggregateVerdict,
			&row.ServiceUnitSource, &row.ServiceUnitCost, &row.CreatedAt, &models,
		); err != nil {
			return nil, fmt.Errorf("advisor: scan billing call: %w", err)
		}
		row.ModelsUsed = []string(models)
		// BYOK heuristic: if any model name carries the user's
		// own provider tag we set BYOKUsed=true. The router
		// records this by suffixing "@byok:provider" when a
		// user-supplied key was honoured. Models without the
		// tag are platform-pool calls.
		for _, m := range row.ModelsUsed {
			if strings.Contains(m, "@byok:") {
				row.BYOKUsed = true
				break
			}
		}
		if p.BYOKOnly && !row.BYOKUsed {
			continue
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("advisor: iter billing calls: %w", err)
	}
	return out, nil
}

// pqStringArray is a tiny adapter for TEXT[] columns. Avoids the
// extra lib/pq dependency hop just to scan one column.
type pqStringArray []string

func (a *pqStringArray) Scan(src any) error {
	if src == nil {
		*a = nil
		return nil
	}
	var raw string
	switch v := src.(type) {
	case []byte:
		raw = string(v)
	case string:
		raw = v
	default:
		return fmt.Errorf("advisor: pqStringArray: unsupported type %T", src)
	}
	raw = strings.TrimSpace(raw)
	if raw == "{}" || raw == "" {
		*a = nil
		return nil
	}
	raw = strings.TrimPrefix(raw, "{")
	raw = strings.TrimSuffix(raw, "}")
	if raw == "" {
		*a = nil
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		p = strings.Trim(p, "\"")
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	*a = out
	return nil
}

func stringSliceOrEmpty(in []string) []string {
	if len(in) == 0 {
		return []string{}
	}
	return in
}

func mapOrEmpty(in map[string]any) map[string]any {
	if in == nil {
		return map[string]any{}
	}
	return in
}

// dedupAndTrim removes whitespace-only entries and duplicates from
// a TEXT[] column. Postgres lets a row carry duplicates but the
// service treats them as a contract violation — quieter to clean
// once on read than to scatter checks downstream.
func dedupAndTrim(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		t := strings.TrimSpace(s)
		if t == "" {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

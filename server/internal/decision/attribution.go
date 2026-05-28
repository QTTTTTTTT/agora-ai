package decision

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

// G1 #2: per-plan decision-block attribution skeleton.
//
// The wiring layer constructs a Contributions struct from
//   (Trace, PMReasoningText)
// after the LLM PM has produced its plan, then persists the
// JSON via PlanRepo.SetBlockContributions. The schema is
// intentionally flat — no nested per-symbol attribution yet;
// that's a Sprint H candidate once we have a few weeks of
// realised PnL to correlate against.
//
// Contract:
//   - Present  : block names whose signal was carried in the
//                DecisionInput (mirrors Trace.PresentBlocks()).
//   - Absent   : block names that were NOT carried (mirrors
//                Trace.AbsentBlocks()).
//   - Cited    : block names mentioned by the PM in its
//                Reasoning. Case-insensitive whole-word match
//                against the canonical block vocabulary, plus
//                a small set of common abbreviations the PM
//                tends to use ("ATR", "TSMOM", "QMJ", "HML",
//                "BAB", "PEAD").
//   - Counts   : per-block row counts from Trace.Counts, only
//                emitted for blocks whose count is non-zero so
//                the JSON stays small.
//   - Signature: short fingerprint string the dashboard can
//                groupby — "pres=A|B|C;cited=A" — so two plans
//                with the same block profile are easy to bucket.

// Contributions is the persisted shape. Field tags are camelCase
// to match the rest of the JSONB conventions in the repo.
type Contributions struct {
	Present   []string       `json:"present"`
	Absent    []string       `json:"absent"`
	Cited     []string       `json:"cited"`
	Counts    map[string]int `json:"counts,omitempty"`
	Signature string         `json:"signature,omitempty"`
}

// BuildContributions assembles the Contributions struct from a
// Trace + free-form Reasoning text. Pure; no I/O. Designed to
// be cheap (single regex pass + a few map allocations) so the
// wiring layer can call it inline on the decision-write path
// without measurable overhead.
func BuildContributions(trace Trace, reasoning string) Contributions {
	present := trace.PresentBlocks()
	absent := trace.AbsentBlocks()
	cited := extractCitedBlocks(reasoning, present)
	counts := nonZeroCounts(trace.Counts)
	return Contributions{
		Present:   present,
		Absent:    absent,
		Cited:     cited,
		Counts:    counts,
		Signature: buildSignature(present, cited),
	}
}

// MarshalJSON exposes the Contributions as the on-disk JSONB
// payload. Returns ("{}", nil) on the zero value so the
// SetBlockContributions writer's soft-fail "no-op on empty"
// stays consistent.
func (c Contributions) MarshalJSON() ([]byte, error) {
	type alias Contributions
	return json.Marshal(alias(c))
}

// EncodeToJSON marshals + returns the raw bytes — convenience
// for the wiring layer which doesn't need to handle the
// alias-trick internally. Returns (nil, err) on encoder error;
// the caller treats nil bytes as "skip the write" (soft-fail).
func (c Contributions) EncodeToJSON() ([]byte, error) {
	return json.Marshal(c)
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// citationVocabulary maps the canonical block name (the strings
// that flow through Trace.PresentBlocks / AbsentBlocks) to the
// REGEX ALTERNATIVES the PM is likely to write in prose. Keeping
// this in one place means a new block (e.g. Sprint G factor
// addition) needs ONE map entry, not a regex sprinkled across
// the codebase.
//
// The PM is allowed to drop the camelCase ("quality scores",
// "value scores"), use academic shorthand ("QMJ", "HML",
// "BAB", "PEAD"), or refer to the metric the block carries
// ("ATR", "TSMOM 12-1m"). Every plausible phrasing maps back
// to one canonical key.
// citationVocabulary carries BOTH English and Chinese aliases —
// the PM writes in whichever language the input was in (see the
// system prompt's locale rule), so missing one half of the
// vocabulary silently drops attribution coverage. Chinese
// aliases were added after the first live decision showed
// `n_cited=0` despite the LLM clearly naming blocks like
// "动量排名 Q1" and "低Beta得分".
var citationVocabulary = map[string][]string{
	"roundtableStance":   {`roundtable\s*stance`, `roundtable verdict`, `圆桌`, `辩论结论`, `辩论\s*verdict`},
	"bullCase":           {`bull\s*case`, `bullish thesis`, `多头\s*观点`, `多头\s*论点`, `看多`},
	"bearCase":           {`bear\s*case`, `bearish thesis`, `空头\s*观点`, `空头\s*论点`, `看空`},
	"quantCase":          {`quant\s*case`, `quant view`, `量化\s*观点`, `量化\s*case`},
	"symbolVerdicts":     {`symbol\s*verdicts?`, `per-symbol verdict`, `个股\s*verdict`, `辩论\s*verdict`, `per-?symbol\s*结论`, `分歧\s*票`, `异议\s*票`, `dissent\s*votes?`, `个股\s*辩论`},
	"fundamentalSummary": {`fundamental\s*summary`, `fundamentals`, `基本面`, `基本面\s*摘要`},
	"sectorRotation":     {`sector\s*rotation`, `sector flow`, `板块\s*轮动`, `行业\s*轮动`},
	"newsSentiment":      {`news\s*sentiment`, `新闻\s*情绪`},
	"sleeveScorecard":    {`sleeve\s*scorecard`, `attribution scorecard`, `归因\s*记分卡`, `策略\s*记分卡`},
	"lessonReplay":       {`lesson\s*replay`, `replay window`, `复盘`, `教训\s*回放`},
	"instrumentHints":    {`instrument\s*hints?`, `标的\s*hint`, `工具\s*提示`},
	"quantSnapshots":     {`quant\s*snapshots?`, `\bATR\b`, `\bMACD\b`, `\bKDJ\b`, `\bRSI\b`, `regime`, `量化\s*快照`, `量化快照`, `仓位.?上限`, `positionSizeCeiling`, `atrPct`},
	"universeRanking":    {`universe\s*ranking`, `cross-?sectional rank`, `动量\s*排名`, `横截面\s*排名`, `宇宙\s*排名`, `Q[1-4]\s*排名`, `排名\s*Q[1-4]`, `quartile`, `compositeZ`},
	"qualityScores":      {`quality\s*scores?`, `\bQMJ\b`, `profitabilityZ`, `safetyZ`, `质量\s*得分`, `质量\s*因子`},
	"valueScores":        {`value\s*scores?`, `\bHML\b`, `book.?to.?price`, `earnings.?to.?price`, `价值\s*得分`, `价值\s*因子`, `市净率\s*z`, `市盈率\s*z`},
	"lowBetaScores":      {`low.?beta\s*scores?`, `\bBAB\b`, `low-?vol`, `低\s*beta`, `低Beta`, `低波`, `防御.?敞口`, `防御.?倾向`},
	"pead":               {`\bPEAD\b`, `post.?earnings\s*drift`, `earnings\s*drift`, `财报后\s*漂移`, `财报漂移`, `盈利\s*漂移`},
	"cooldowns":          {`cool\s*downs?`, `re-?entry lock`, `冷却`, `冷却\s*期`, `再入场\s*锁`},
	"riskBudget":         {`risk\s*budget`, `drawdown\s*throttle`, `vol\s*target`, `风险\s*预算`, `回撤\s*throttle`, `回撤\s*限速`, `波动\s*目标`, `volScalar`, `ddScalar`},
	"newsCatalysts":      {`news\s*catalysts?`, `catalyst block`, `新闻\s*催化`, `新闻\s*事件`, `催化剂`, `主题\s*资讯`},
	"earningsCalendar":   {`earnings\s*calendar`, `upcoming earnings`, `财报\s*日历`, `财报\s*日`, `即将\s*财报`},
	"exposure":           {`exposure\s*snapshot`, `concentration`, `sector cap`, `敞口`, `集中度`, `单票\s*上限`, `行业\s*上限`},
	"correlations":       {`correlations?\b`, `\brho\b`, `high-?corr`, `相关性`, `相关\s*矩阵`, `高相关`},
	"pairSpreads":        {`pair\s*spreads?`, `spread.?Z`, `pairs trade`, `配对\s*价差`, `价差\s*z`, `配对\s*交易`},
}

// compiledCitationPatterns is the lazily-built regex per block.
// Building once at package init keeps the per-plan attribution
// cost negligible. We use case-insensitive, multi-line flags.
var compiledCitationPatterns = compileCitationPatterns()

func compileCitationPatterns() map[string]*regexp.Regexp {
	out := make(map[string]*regexp.Regexp, len(citationVocabulary))
	for block, alts := range citationVocabulary {
		// Join alternatives with `|` and wrap in (?i) for
		// case insensitivity. No word-boundary on the OUTSIDE
		// because the alternatives themselves carry word
		// boundaries where appropriate (\bATR\b etc.).
		pattern := `(?i)(?:` + strings.Join(alts, "|") + `)`
		out[block] = regexp.MustCompile(pattern)
	}
	return out
}

// extractCitedBlocks runs the per-block regex against the
// reasoning text and returns the canonical names whose pattern
// matched at least once. The output is sorted by the order they
// appear in `present` (so present-and-cited blocks land first),
// then alphabetically for any present=false / cited=true row
// (which is itself a flag — PM cited a block the wiring didn't
// actually carry, suggesting prompt drift).
//
// Returns an empty slice (never nil) on no matches so the
// downstream JSON encoder produces `"cited": []` rather than
// `"cited": null`.
func extractCitedBlocks(reasoning string, present []string) []string {
	if strings.TrimSpace(reasoning) == "" {
		return []string{}
	}
	hits := make(map[string]bool, len(compiledCitationPatterns))
	for block, pattern := range compiledCitationPatterns {
		if pattern.MatchString(reasoning) {
			hits[block] = true
		}
	}
	if len(hits) == 0 {
		return []string{}
	}
	// Two-pass output: first present-ordered cited blocks, then
	// any alphabetically-sorted residue (cited-but-absent).
	out := make([]string, 0, len(hits))
	seen := make(map[string]bool, len(hits))
	for _, block := range present {
		if hits[block] {
			out = append(out, block)
			seen[block] = true
		}
	}
	residue := make([]string, 0)
	for block := range hits {
		if !seen[block] {
			residue = append(residue, block)
		}
	}
	sort.Strings(residue)
	out = append(out, residue...)
	return out
}

// nonZeroCounts strips zero-count entries from SignalCounts so
// the persisted JSON stays small. SignalCounts is a struct (not
// a map) so we hand-translate; the field names match the JSON
// tags on SignalCounts.
func nonZeroCounts(c SignalCounts) map[string]int {
	out := make(map[string]int)
	addNonZero := func(k string, v int) {
		if v != 0 {
			out[k] = v
		}
	}
	addNonZero("universe", c.Universe)
	addNonZero("positions", c.Positions)
	addNonZero("instrumentHints", c.InstrumentHints)
	addNonZero("quantSnapshots", c.QuantSnapshots)
	addNonZero("universeRanking", c.UniverseRanking)
	addNonZero("qualityScores", c.QualityScores)
	addNonZero("valueScores", c.ValueScores)
	addNonZero("lowBetaScores", c.LowBetaScores)
	addNonZero("peadSignals", c.PEADSignals)
	addNonZero("cooldowns", c.Cooldowns)
	addNonZero("newsCatalysts", c.NewsCatalysts)
	addNonZero("earningsCalendar", c.EarningsCalendar)
	addNonZero("exposureBreaches", c.ExposureBreaches)
	addNonZero("correlationsHigh", c.CorrelationsHigh)
	addNonZero("corrCandidates", c.CorrCandidates)
	addNonZero("pairSpreads", c.PairSpreads)
	if len(out) == 0 {
		return nil
	}
	return out
}

// buildSignature produces a short, deterministic "pres=A|B|C;
// cited=A" string the dashboard can groupby to find plans with
// the same block profile. Used for back-of-envelope clustering
// — not a primary key — so we accept the small chance of two
// distinct block profiles colliding (the GIN index on the
// JSONB column handles exact-shape lookups when needed).
func buildSignature(present, cited []string) string {
	if len(present) == 0 && len(cited) == 0 {
		return ""
	}
	return "pres=" + strings.Join(present, "|") + ";cited=" + strings.Join(cited, "|")
}

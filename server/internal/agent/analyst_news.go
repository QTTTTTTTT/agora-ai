// analyst_news.go — S8.1 NewsAnalyst.
//
// Reads AnalystInput.News (raw headline feed + material-event
// tags) and produces a catalyst-focused report. Where the
// SentimentAnalyst is about crowd mood, the NewsAnalyst is
// about "what just happened to this company" — earnings,
// M&A, regulator actions, downgrades.
//
// The directional anchor is derived from the material-event tags
// (which the wiring layer extracted via keyword spotting) plus
// the headline density: a quiet day defaults to neutral / low
// conviction, no matter what the LLM says.

package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// NewsAnalyst implements AnalystAgent for narrative catalyst
// detection. Construct via NewNewsAnalyst.
type NewsAnalyst struct {
	*analystBase
}

// NewNewsAnalyst constructs a NewsAnalyst.
func NewNewsAnalyst(id, name, fundID string, llm LLMClient, opts ...AnalystOption) *NewsAnalyst {
	return &NewsAnalyst{analystBase: newAnalystBase(id, name, fundID, llm, opts...)}
}

// Category returns CategoryNews.
func (a *NewsAnalyst) Category() AnalystCategory { return CategoryNews }

// Analyze produces a news-focused AnalystReport. Direction comes
// from the polarity of the material-event tags (m_and_a +
// regulator_action_neg → bearish; upgrade + earnings_beat →
// bullish). The LLM thesis is consulted on top.
func (a *NewsAnalyst) Analyze(ctx context.Context, input AnalystInput) (AnalystReport, error) {
	if strings.TrimSpace(input.Symbol) == "" {
		return AnalystReport{}, errors.New("news: input.Symbol required")
	}
	dir, conf := scoreNewsDirection(input)
	findings, risks := summariseNews(input)

	rep := AnalystReport{
		AgentID:     a.id,
		AgentName:   a.name,
		Category:    CategoryNews,
		Symbol:      input.Symbol,
		AsOf:        input.AsOf,
		GeneratedAt: a.now(),
		Direction:   dir,
		Confidence:  conf,
		Thesis:      newsFallbackThesis(input, dir, conf),
		KeyFindings: findings,
		Risks:       risks,
		DataPoints:  newsDataPoints(input),
		LLMModel:    "fallback",
	}

	if a.llm != nil {
		sys := a.buildSystemPrompt()
		user := a.buildUserPrompt(input, dir, conf)
		if parsed, err := a.callLLMForReport(ctx, sys, user); err == nil {
			rep.Direction = mergeDirectionWithRule(dir, normaliseDirection(parsed.Direction))
			if parsed.Confidence > 0 {
				rep.Confidence = clampConfidence(parsed.Confidence)
			}
			if t := strings.TrimSpace(parsed.Thesis); t != "" {
				rep.Thesis = t
			}
			if len(parsed.KeyFindings) > 0 {
				rep.KeyFindings = parsed.KeyFindings
			}
			if len(parsed.Risks) > 0 {
				rep.Risks = parsed.Risks
			}
			rep.LLMModel = "llm"
		} else {
			a.logger.Warn("news analyst: LLM failed, using fallback",
				"err", err, "symbol", input.Symbol)
		}
	}

	if input.News != nil {
		for _, h := range input.News.Headlines {
			if h.URL != "" {
				rep.Sources = append(rep.Sources, h.URL)
				if len(rep.Sources) >= 5 {
					break
				}
			}
		}
	}
	if err := rep.Validate(); err != nil {
		return AnalystReport{}, err
	}
	return rep, nil
}

// --- internal helpers ------------------------------------------------------

// eventTagPolarity maps each detected event tag to a {-1, 0, +1}
// directional vote and a conviction weight. Wiring-layer keyword
// spotting can add new tags; this map is the single source of
// truth for the analyst's interpretation.
var eventTagPolarity = map[string]struct {
	Sign   int
	Weight int // 1..3
}{
	"earnings_beat":         {+1, 3},
	"earnings_miss":         {-1, 3},
	"guidance_raise":        {+1, 3},
	"guidance_cut":          {-1, 3},
	"m_and_a_acquirer":      {+1, 2},
	"m_and_a_target":        {+1, 3},
	"upgrade":               {+1, 2},
	"downgrade":             {-1, 2},
	"regulator_action_pos":  {+1, 1},
	"regulator_action_neg":  {-1, 3},
	"product_launch":        {+1, 1},
	"recall":                {-1, 2},
	"lawsuit_loss":          {-1, 2},
	"lawsuit_win":           {+1, 1},
	"insider_buy":           {+1, 1},
	"insider_sell":          {-1, 1},
	"buyback_announce":      {+1, 2},
	"dividend_increase":     {+1, 1},
	"dividend_cut":          {-1, 2},
	"capital_raise_dilutive": {-1, 2},
}

func scoreNewsDirection(input AnalystInput) (Direction, int) {
	if input.News == nil || (len(input.News.Headlines) == 0 && len(input.News.MaterialEventTags) == 0) {
		return DirectionNeutral, 20
	}
	score := 0
	weights := 0
	for _, tag := range input.News.MaterialEventTags {
		if p, ok := eventTagPolarity[strings.ToLower(strings.TrimSpace(tag))]; ok {
			score += p.Sign * p.Weight
			weights += p.Weight
		}
	}
	if weights == 0 {
		// Headlines exist but no material tags → low-conviction neutral.
		conf := clampConfidence(20 + len(input.News.Headlines)*2)
		return DirectionNeutral, conf
	}
	normalised := float64(score) / float64(weights) // [-1, 1]
	conf := int(absFloat(normalised) * 80)          // tags rarely give 100
	conf = clampConfidence(conf)
	switch {
	case normalised >= 0.2:
		return DirectionBullish, conf
	case normalised <= -0.2:
		return DirectionBearish, conf
	default:
		return DirectionNeutral, conf
	}
}

func summariseNews(input AnalystInput) (findings, risks []string) {
	if input.News == nil || len(input.News.Headlines) == 0 {
		findings = append(findings, "no news in window; analyst sitting out")
		return findings, risks
	}
	if len(input.News.MaterialEventTags) > 0 {
		findings = append(findings,
			fmt.Sprintf("material events: %s", strings.Join(input.News.MaterialEventTags, ", ")))
	}
	// Show up to the 3 most-recent headlines.
	for i, h := range input.News.Headlines {
		if i >= 3 {
			break
		}
		entry := fmt.Sprintf("%s [%s, %s]",
			truncateRunes(h.Title, 120), h.Source, formatHeadlineTime(h.PublishedAt))
		findings = append(findings, entry)
	}
	// Any tag with negative polarity goes into risks.
	for _, tag := range input.News.MaterialEventTags {
		if p, ok := eventTagPolarity[strings.ToLower(tag)]; ok && p.Sign < 0 {
			risks = append(risks, fmt.Sprintf("negative event: %s", tag))
		}
	}
	return findings, risks
}

func newsDataPoints(input AnalystInput) []DataPoint {
	var dp []DataPoint
	if input.News == nil {
		return dp
	}
	dp = append(dp, DataPoint{Name: "news.headline_count", Value: fmt.Sprintf("%d", len(input.News.Headlines))})
	if len(input.News.MaterialEventTags) > 0 {
		dp = append(dp, DataPoint{
			Name:  "news.material_tags",
			Value: strings.Join(input.News.MaterialEventTags, ","),
		})
	}
	return dp
}

func newsFallbackThesis(input AnalystInput, dir Direction, conf int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "News view on %s: %s (confidence %d%%). ", input.Symbol, dir, conf)
	if input.News == nil || len(input.News.Headlines) == 0 {
		b.WriteString("No headlines in the window.")
		return b.String()
	}
	if len(input.News.MaterialEventTags) > 0 {
		fmt.Fprintf(&b, "Material event tags: %s.", strings.Join(input.News.MaterialEventTags, ", "))
	} else {
		fmt.Fprintf(&b, "%d headlines, no material event tags.", len(input.News.Headlines))
	}
	return b.String()
}

func formatHeadlineTime(t time.Time) string {
	if t.IsZero() {
		return "unknown time"
	}
	return t.UTC().Format("2006-01-02 15:04Z")
}

func (a *NewsAnalyst) buildSystemPrompt() string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are %s, a news / catalyst analyst on fund %s. ", a.name, a.fundID)
	b.WriteString("Read the headline feed + the pre-tagged material events; flag the catalysts that ")
	b.WriteString("actually move price (earnings, guidance, M&A, regulator, downgrades). Ignore generic ")
	b.WriteString("market commentary and any social-mood items — those are other analysts' jobs.")
	if a.persona != "" {
		fmt.Fprintf(&b, " Persona: %s.", a.persona)
	}
	b.WriteString("\n\nReturn ONLY a JSON object with this exact shape, no markdown:")
	b.WriteString(`
{
  "direction": "bullish" | "bearish" | "neutral",
  "confidence": <int 0-100>,
  "thesis": "<one-paragraph>",
  "key_findings": ["<bullet>", ...],
  "risks": ["<bullet>", ...]
}`)
	return b.String()
}

func (a *NewsAnalyst) buildUserPrompt(input AnalystInput, dir Direction, conf int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Symbol: %s\n", input.Symbol)
	fmt.Fprintf(&b, "Rule-based prior: %s, confidence %d%%\n\n", dir, conf)
	if input.News == nil || len(input.News.Headlines) == 0 {
		b.WriteString("No headlines in window.\n")
		return b.String()
	}
	if len(input.News.MaterialEventTags) > 0 {
		fmt.Fprintf(&b, "Material event tags: %s\n\n", strings.Join(input.News.MaterialEventTags, ", "))
	}
	b.WriteString("Headlines (most-recent first):\n")
	for i, h := range input.News.Headlines {
		if i >= 10 {
			break
		}
		fmt.Fprintf(&b, "  - [%s, %s] %s\n",
			h.Source, formatHeadlineTime(h.PublishedAt), truncateRunes(h.Title, 140))
		if strings.TrimSpace(h.Summary) != "" {
			fmt.Fprintf(&b, "    %s\n", truncateRunes(h.Summary, 200))
		}
	}
	if strings.TrimSpace(input.Notes) != "" {
		fmt.Fprintf(&b, "\nOperator notes: %s\n", input.Notes)
	}
	return b.String()
}

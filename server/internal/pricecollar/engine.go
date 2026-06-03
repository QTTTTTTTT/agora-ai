package pricecollar

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// Engine orchestrates the collar rule. Construction-time stateless;
// safe to share across goroutines.
type Engine struct {
	source ReferenceSource
	opts   EngineOptions
}

// NewEngine constructs the engine. The reference source MUST be
// non-nil — pass NoOpReferenceSource if you want to bypass the
// gate (useful for tests / migration windows). nil source returns
// an error so misconfiguration fails loud at wiring time.
func NewEngine(source ReferenceSource, opts EngineOptions) (*Engine, error) {
	if source == nil {
		return nil, errors.New("pricecollar: reference source required (use NoOpReferenceSource to disable)")
	}
	return &Engine{
		source: source,
		opts:   opts.applyDefaults(),
	}, nil
}

// Check evaluates the probe and emits a verdict.
//
// Decision logic:
//
//  1. probe.IntendedPrice <= 0 → market order → always Allow.
//     Limit price is the only thing this gate guards; market
//     orders inherit the matcher's quote which already lives
//     inside the marketstatus calendar / stale gate.
//  2. Reference unavailable (source returns nil or stale) →
//     emit NoReferenceDecision (default warn).
//  3. |intended - reference| / reference > threshold → reject.
//  4. Otherwise → allow.
func (e *Engine) Check(ctx context.Context, probe Probe) (*CheckResult, error) {
	if e == nil {
		return nil, errors.New("pricecollar: nil engine")
	}
	if strings.TrimSpace(probe.InstrumentKey) == "" && strings.TrimSpace(probe.Symbol) == "" {
		return nil, fmt.Errorf("%w: instrument_key or symbol required", ErrInvalidProbe)
	}

	now := e.opts.Now()
	res := &CheckResult{
		Decision:            DecisionAllow,
		AppliedThresholdBps: ResolveThresholdBps(e.opts, probe),
	}

	// 1) market order short-circuit
	if probe.IntendedPrice <= 0 {
		return res, nil
	}

	// 2) resolve reference
	ref, refErr := e.source.GetReferenceQuote(ctx, probe)
	if refErr != nil {
		// Source error is fail-OPEN to match marketstatus's
		// philosophy: misconfiguration of the gate must not
		// become a denial of service. We still mark the event
		// so operators see the failure.
		res.Events = append(res.Events, Event{
			RuleCode:        RuleNoReference,
			Decision:        DecisionWarn,
			Summary:         fmt.Sprintf("reference quote lookup failed for %s: %v", probe.Symbol, refErr),
			Metadata:        map[string]any{"lookup_error": refErr.Error()},
			DetectedAt:      now,
			DetectorVersion: detectorVersion,
		})
		res.Decision = mergeDecision(res.Decision, DecisionWarn)
		return res, nil
	}
	if ref == nil || ref.Price <= 0 {
		res.Events = append(res.Events, Event{
			RuleCode:        RuleNoReference,
			Decision:        e.opts.NoReferenceDecision,
			Summary:         fmt.Sprintf("no usable reference quote for %s; price-collar check skipped", probe.Symbol),
			Metadata: map[string]any{
				"reason": "missing",
			},
			DetectedAt:      now,
			DetectorVersion: detectorVersion,
		})
		res.Decision = mergeDecision(res.Decision, e.opts.NoReferenceDecision)
		return res, nil
	}
	res.Reference = ref

	// 3) freshness check
	if e.opts.MaxReferenceAge > 0 && !ref.AsOf.IsZero() {
		age := now.Sub(ref.AsOf)
		if age < 0 {
			age = -age
		}
		if age > e.opts.MaxReferenceAge {
			res.Events = append(res.Events, Event{
				RuleCode:        RuleNoReference,
				Decision:        e.opts.NoReferenceDecision,
				Summary:         fmt.Sprintf("reference quote for %s is %s old (max %s); price-collar check skipped", probe.Symbol, age.Round(time.Second), e.opts.MaxReferenceAge),
				Metadata: map[string]any{
					"reason":            "stale",
					"reference_age_sec": int64(age.Seconds()),
					"max_age_sec":       int64(e.opts.MaxReferenceAge.Seconds()),
				},
				DetectedAt:      now,
				DetectorVersion: detectorVersion,
			})
			res.Decision = mergeDecision(res.Decision, e.opts.NoReferenceDecision)
			return res, nil
		}
	}

	// 4) collar comparison
	deviation := math.Abs(probe.IntendedPrice-ref.Price) / ref.Price
	deviationBps := int(math.Round(deviation * 10000))
	if deviationBps > res.AppliedThresholdBps {
		res.Events = append(res.Events, Event{
			RuleCode: RulePriceCollar,
			Decision: DecisionReject,
			Summary: fmt.Sprintf(
				"limit price %.4f deviates %s from reference %.4f (cap %s) — looks like a fat-finger or bad-quote bug, rejecting",
				probe.IntendedPrice,
				formatBpsPct(deviationBps),
				ref.Price,
				formatBpsPct(res.AppliedThresholdBps),
			),
			Metadata: map[string]any{
				"intended_price":   probe.IntendedPrice,
				"reference_price":  ref.Price,
				"deviation_bps":    deviationBps,
				"threshold_bps":    res.AppliedThresholdBps,
				"reference_as_of":  ref.AsOf,
				"asset_class":      probe.AssetClass,
				"market":           probe.Market,
				"side":             probe.Side,
			},
			DetectedAt:      now,
			DetectorVersion: detectorVersion,
		})
		res.Decision = DecisionReject
		return res, nil
	}

	// Allow path. We don't emit an event on allow — keeping the
	// happy-path event stream silent matches marketstatus's
	// convention and saves the audit table from a flood of allow
	// rows.
	return res, nil
}

// mergeDecision combines two decisions: reject dominates warn,
// warn dominates allow.
func mergeDecision(a, b Decision) Decision {
	if a == DecisionReject || b == DecisionReject {
		return DecisionReject
	}
	if a == DecisionWarn || b == DecisionWarn {
		return DecisionWarn
	}
	return DecisionAllow
}

// NoOpReferenceSource always returns (nil, nil). Useful for tests
// or rollout windows where you want the gate plumbed but inert.
type NoOpReferenceSource struct{}

func (NoOpReferenceSource) GetReferenceQuote(_ context.Context, _ Probe) (*ReferenceQuote, error) {
	return nil, nil
}

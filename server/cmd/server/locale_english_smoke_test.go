package main

// locale_english_smoke_test.go — guardrail against Chinese leaking into
// LLM prompts emitted under en-US locale.
//
// Step 12 of the full English rollout. master_agent already had a
// dedicated smoke test (master_agent_english_smoke_test.go) since
// commit ccb4369; this file extends the same pattern to the prompts
// migrated in Step 4:
//
//   - ab_shadow_bside.bSideSystemPromptFor(LocaleEN)
//   - ab_shadow_bside.bSideRecapSystemPromptFor(LocaleEN)
//   - agent_self_learning_prompts.agentLearningPromptParts(en-US ctx)
//   - agent_self_learning_prompts.roleSpecificSystemHintFor(en-US, ...)
//
// fund_assist lives in internal/api and has its own smoke test there;
// tactic_agent's prompt is gated behind the cn_tactics persona files
// (which are zh-only by design) and is exercised by the package-local
// tests in internal/agent.
//
// The assertion is intentionally crude: zero CJK ideographs in the
// rendered prompt. False negatives are impossible (any zh leak fails
// the test); false positives only happen if a prompt deliberately
// shows the user a Chinese token — none of the prompts above need to.

import (
	"context"
	"testing"
	"unicode"

	"github.com/fundai/server/internal/i18nmsg"
)

func TestAgentLearningPromptEnglishSmoke(t *testing.T) {
	ctx := i18nmsg.WithLocale(context.Background(), i18nmsg.LocaleEN)
	for _, role := range []string{"pm", "researcher", "trader", "risk", ""} {
		system, userTail := agentLearningPromptParts(ctx, role)
		assertNoCJK(t, "agentLearning system role="+role, system)
		assertNoCJK(t, "agentLearning userTail role="+role, userTail)
	}
}

func TestABShadowPromptEnglishSmoke(t *testing.T) {
	assertNoCJK(t, "bSideSystemPromptFor(en)", bSideSystemPromptFor(i18nmsg.LocaleEN))
	assertNoCJK(t, "bSideRecapSystemPromptFor(en)", bSideRecapSystemPromptFor(i18nmsg.LocaleEN))
}

func TestRoleSpecificSystemHintEnglishSmoke(t *testing.T) {
	for _, role := range []string{"pm", "researcher", "trader", "risk", ""} {
		hint := roleSpecificSystemHintFor(i18nmsg.LocaleEN, role)
		assertNoCJK(t, "roleSpecificSystemHintFor(en) role="+role, hint)
	}
}

// assertNoCJK fails t when s contains any CJK ideograph. The message
// pinpoints which prompt section leaked so the on-call can grep for
// the offending template.
func assertNoCJK(t *testing.T, label, s string) {
	t.Helper()
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			t.Errorf("%s contains CJK ideograph %q in: %q", label, string(r), s)
			return
		}
	}
}

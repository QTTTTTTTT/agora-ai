// master_agent_english_smoke_test.go — regression guard for the
// 2026-06-11 fix that flipped MasterAgent prompts from Chinese
// to English so en-US UI users get English Master Panel Reports.
//
// Pre-fix the prompt was almost entirely Chinese ("你是 X，请按
// 上述 persona 分析下面这只股票..." plus 9 numbered Chinese rules
// and a Chinese output schema). After the fix, the only Chinese
// text the prompt should carry is:
//
//   * the persona's name_zh (e.g. "巴菲特") as a parenthetical
//     identity label
//   * the persona JSON dump in === PERSONA JSON === (philosophy /
//     must_have_criteria / red_lines were authored in Chinese
//     and deliberately left as-is — rule 10 instructs the LLM to
//     read them as Chinese context but respond in English)
//
// Anything else in Chinese is a regression. We grep for the
// specific instruction-language phrases the old prompt used so a
// drive-by edit that re-introduces "你是 / 请按 / 数据缺失" type
// strings fails CI immediately.
package agent

import (
	"strings"
	"testing"
)

// forbiddenInstructionPhrases are Chinese prompt words that the
// pre-2026-06-11 prompt carried directly in its instructions /
// schema / fallback messages. None of them are persona-derived;
// every entry was authored in master_agent.go itself.
var forbiddenInstructionPhrases = []string{
	// System-prompt verb-of-instruction openers
	"你是",
	"你必须",
	"请按",
	"你的核心",
	"你的典型持有期",
	// Schema instruction language (would tell the LLM to write
	// Chinese — the exact failure mode the user reported)
	"中文论述",
	"中文回答",
	// User-prompt data-block labels
	"数据缺失",
	"未提供",
	"未计算",
	"年报口径",
	"季报",
	// Old rule 5 / 6 anchors
	"最新季度",
	"近期",
	"增收不增利",
	"增利不增收",
	"百分点",
	// Old fallback theses
	"暂无足够数据",
	"模型输出未通过校验",
	"保守给出",
}

func TestMasterPrompts_NoForbiddenChineseInstructions(t *testing.T) {
	a, err := NewMasterAgent(newTestPersona(), nil)
	if err != nil {
		t.Fatalf("NewMasterAgent: %v", err)
	}
	sys := a.buildSystemPrompt()
	user := a.buildUserPrompt(MasterInput{Symbol: "AAPL"})

	for _, surface := range []struct {
		name string
		body string
	}{
		{"system_prompt", sys},
		{"user_prompt", user},
	} {
		for _, phrase := range forbiddenInstructionPhrases {
			if strings.Contains(surface.body, phrase) {
				t.Errorf("%s carries forbidden Chinese instruction phrase %q", surface.name, phrase)
			}
		}
	}
}

// TestMasterPrompts_HaveExplicitEnglishContract pins the
// English-output rule (rule 10) and the schema description so a
// drive-by edit can't silently revert. Without these the model
// gradually drifts back toward Chinese (its persona JSON
// vocabulary biases it that way).
func TestMasterPrompts_HaveExplicitEnglishContract(t *testing.T) {
	a, err := NewMasterAgent(newTestPersona(), nil)
	if err != nil {
		t.Fatalf("NewMasterAgent: %v", err)
	}
	sys := a.buildSystemPrompt()
	for _, mustHave := range []string{
		// Rule 10 anchor — the explicit terminal instruction.
		"RESPOND IN ENGLISH",
		// Schema-level English declarations.
		"a single English paragraph",
		"the 3 most decisive judgement factors, each in English",
		"the 2 most material risks, each in English",
		// Rule 10's red-line carve-out — without this the LLM
		// might translate red-line entries to English and break
		// the downstream exact-match dedup.
		"red_lines_hit entries MUST be quoted verbatim",
	} {
		if !strings.Contains(sys, mustHave) {
			t.Errorf("system prompt missing required English contract phrase %q", mustHave)
		}
	}
}

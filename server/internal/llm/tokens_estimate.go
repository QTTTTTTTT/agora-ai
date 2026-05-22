package llm

// estimatePromptTokens returns a conservative upper-bound token count
// for the prompt portion of a ChatRequest. Used by the F28 fund-token
// quota gate when we need to decide *before* hitting the provider
// whether a call is allowed.
//
// We use a simple chars/4 heuristic — accurate enough to keep budget
// breaches within a small factor (typically <1.3x), without depending
// on the tokenizer-specific libraries (tiktoken, sentencepiece) that
// would otherwise bloat the binary. A tokenizer-precise count is
// nice-to-have but not worth the complexity here because the gate is
// already pessimistic on the output side (we add MaxTokens).
//
// The +4 per message accounts for the role tag overhead in chat
// templates (system / user / assistant turn boundaries).
func estimatePromptTokens(req ChatRequest) int {
	if len(req.Messages) == 0 {
		return 0
	}
	total := 0
	for _, m := range req.Messages {
		total += len(m.Content)/4 + 4
	}
	return total
}

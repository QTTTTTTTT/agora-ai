## Findings

The PM Agent's LLM prompt construction logic is in `/Users/bytedance/Downloads/ai-fund-platform-v3-full/server/internal/agent/pm.go`. 
The methods of interest are:
- `buildSystemPrompt` (lines 1089-1106)
- `buildPlanPrompt` (lines 1108-1141)
- The method calling these is `llmReasoning` (lines 1019-1049)

### Context Injection Analysis

The `buildPlanPrompt` function receives several context arrays via the `PlanInput` struct and injects them directly into the prompt without any limits or truncation. 

1.  **Holdings:**
    ```go
    	for _, h := range input.Holdings {
    		fmt.Fprintf(&sb, "- %s: %d shares @ $%.2f (P&L: %.1f%%)\n", h.Symbol, h.Quantity, h.AvgCost, h.PnLPct)
    	}
    ```
    *Issue:* No limit on the number of holdings. A large portfolio will consume many tokens.

2.  **Consensus (Research Items):**
    ```go
    	for _, c := range input.Consensus {
    		fmt.Fprintf(&sb, "- %s: %s (%d%% confidence, action: %s) — %s\n",
    			c.Symbol, c.Direction, c.Confidence, c.Action, c.Reasoning)
    	}
    ```
    *Issue:* No limit on the number of consensus items. More importantly, `c.Reasoning` is included entirely without character limits, which could be very long.

3.  **Actions:**
    ```go
    	for _, a := range actions {
    		fmt.Fprintf(&sb, "- %s %s: qty=%d, amount=$%.2f, reason: %s\n",
    			a.Action, a.Symbol, a.Quantity, a.Amount, a.Reasoning)
    	}
    ```
    *Issue:* No limit on the number of proposed actions. `a.Reasoning` is also included entirely.

4.  **MemoryContext:**
    ```go
    	if input.MemoryContext != "" {
    		fmt.Fprintf(&sb, "\n## Recent Memory / Lessons\n%s\n", input.MemoryContext)
    	}
    ```
    *Issue:* The `MemoryContext` string is injected whole. There's no character limit applied before adding it to the prompt.

### Exact File Paths and Function Names for Optimization

**File:** `/Users/bytedance/Downloads/ai-fund-platform-v3-full/server/internal/agent/pm.go`

**Functions Needing Token Optimization/Truncation:**
1.  **`buildPlanPrompt`**: 
    - Needs limits on the number of `Holdings`, `Consensus`, and `Actions` items to include.
    - Needs truncation for long string fields like `ConsensusItem.Reasoning` and `PlanAction.Reasoning`.
    - Needs a character or token limit for `MemoryContext`.
2.  *(Optional but Recommended)* The functions that prepare `PlanInput` (specifically `MemoryContext`) might also need updates to truncate *before* passing to the PM agent, but the immediate injection point is `buildPlanPrompt`.


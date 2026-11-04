package chat

import (
	"fmt"

	"knomit/internal/engine/llm"
)

// USER-FACING COPY. The chat layer owns exactly two sentences the person
// reads that the agent loop deliberately leaves unworded. Each is ONE named
// constant here so it can be reviewed, approved and changed in one place; tests
// assert the constant, never a duplicate literal.

// priceWarningFormat is the near-the-price-cliff warning. Arguments, in order:
// the prompt size in tokens (approximate), the model id, and the token count
// above which the model's per-token price rises (PriceWarning.TierTokens, or
// the warning threshold when the card has a single tier).
const priceWarningFormat = "This chat's context has grown to about %[1]d tokens. %[2]s costs more per token for every request above %[3]d tokens, so each further message will cost noticeably more. Start a new chat to keep the cost down."

// reasoningTruncatedMessage is shown instead of the loop's generic "The reply
// hit the output limit." when the reply that was cut off contained only
// reasoning and no text.
const reasoningTruncatedMessage = "The model ran out of room while thinking, before it wrote a reply. Ask again, or start a new chat if this one has grown long."

// priceWarningText words a structured warning from the loop.
func priceWarningText(w llm.PriceWarning) string {
	tier := w.TierTokens
	if tier <= 0 {
		tier = w.Threshold
	}
	return fmt.Sprintf(priceWarningFormat, w.PromptTokens, w.Model, tier)
}

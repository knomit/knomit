package chat

import (
	"fmt"
	"strconv"
	"strings"

	"knomit/internal/engine/llm"
)

// USER-FACING COPY. The chat layer owns exactly two sentences the person
// reads that the agent loop deliberately leaves unworded. Each is ONE named
// constant here so it can be reviewed, approved and changed in one place; tests
// assert the constant, never a duplicate literal.

// priceWarningFormat is the near-the-price-cliff warning (user-approved wording).
// The one argument is the token count above which the model's per-token price
// rises (PriceWarning.TierTokens, or the warning threshold when the card has a
// single tier), formatted by groupThousands.
const priceWarningFormat = "This chat is nearing %s tokens. Past that, each message costs more. Start a new chat to save."

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
	return fmt.Sprintf(priceWarningFormat, groupThousands(tier))
}

// groupThousands writes n in decimal with a comma before each group of three
// digits, e.g. 100000 becomes "100,000". Negative values keep their sign.
func groupThousands(n int) string {
	if n < 0 {
		return "-" + groupThousands(-n)
	}
	s := strconv.Itoa(n)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

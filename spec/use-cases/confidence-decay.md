# Use Case: Confidence Decay

## Summary

Knomit does not automatically decay fact confidence over time. Instead, the **reading agent** decides how to weight a fact based on its age and domain context.

## Rationale

Not all facts age equally. "Gravity makes objects fall" never decays. "Alice likes rock music" might. Embedding a universal decay timer into the system would require domain-specific decay rates that are essentially unknowable at the system level.

## How It Works

1. An agent retrieves a fact from the knowledge base
2. The agent checks the fact's last commit date via `git log`
3. Based on the domain and the agent's own judgment, it decides:
   - **Trust it as-is** — the fact is stable (e.g., physical laws, geography)
   - **Treat with lower confidence** — the fact is old and the domain is volatile (e.g., personal preferences, market conditions)
   - **Trigger re-verification** — open a new learning moment to re-check the fact against current evidence

## Example

An investment agent reads: "Bob has a strong track record in tech stocks" (confidence: 0.85, last commit: 18 months ago).

The agent notes the age and the volatile domain. Rather than blindly acting on 0.85 confidence, it adjusts its internal weighting downward and may trigger a re-verification learning moment to check Bob's recent performance before making a decision.

## Key Principle

Intelligence belongs at the edges (the reading agent), not in the storage layer (Knomit). Knomit provides the data and the timestamps. The agent provides the judgment.

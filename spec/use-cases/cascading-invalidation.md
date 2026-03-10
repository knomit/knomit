# Use Case: Cascading Invalidation

## Summary

When a base fact is updated or reverted, any higher-order facts that were derived from it may need re-evaluation. Knomit does not automate this — it provides the tools to discover dependencies and leaves the decision to agents.

## How It Works

### Finding dependents

When a fact is modified, an agent can find everything that depends on it by grepping for the original commit hash across all fact files:

```bash
grep -rl "abc1234" --include="*.md" know/
```

This returns every fact whose `refs` contain that commit hash — i.e., every fact that cited the modified fact as evidence.

### Deciding what to do

The agent reviews each dependent fact and decides:

- **Still valid** — the base fact changed but the conclusion still holds (e.g., new evidence reinforced rather than contradicted)
- **Needs re-evaluation** — the base fact changed in a way that might affect the derived conclusion. The agent opens a new learning moment to re-assess.
- **Invalidated** — the base fact was reverted or contradicted, and the derived fact no longer has sufficient evidence. The agent proposes an update or removal via PR.

## Example

1. Fact A: "Alice bought Album X" (commit `abc1234`)
2. Fact B: "Alice likes rock music" (refs: [`abc1234`, `def5678`])
3. Fact A is updated: "Alice returned Album X — it was a gift, not a purchase"
4. Agent greps for `abc1234`, finds Fact B depends on it
5. Agent re-evaluates: Fact B still has `def5678` (concert attendance) as evidence, but confidence should be lowered
6. Agent opens a PR to update Fact B's confidence from 0.85 to 0.6

## Key Principle

Cascading invalidation is an emergent capability of the `refs` format, not a built-in system feature. The data structure makes dependencies discoverable; agents decide what to do about them.

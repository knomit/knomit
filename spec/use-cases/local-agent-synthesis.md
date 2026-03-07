# Use Case: Local Agent Synthesis

## Summary

A Local Agent (or Synthesizer) is an intelligent edge node that transforms raw observations into structured facts. How it decides when and what to synthesize is external to Knomit — Knomit only defines the format and protocol for proposing facts.

## Role in the System

The Local Agent bridges the gap between raw data and structured knowledge:

```
Episodic Store → Local Agent (pattern detection + synthesis) → Knomit PR
```

## How It Works

1. **Polling** — The agent periodically sweeps an episodic data source for unprocessed events
2. **Pattern detection** — Using an LLM or rule-based logic, the agent looks for:
   - Repeating patterns across multiple events
   - Critical mass of evidence supporting a conclusion
   - Significant single events worth recording as facts
3. **Synthesis** — When a pattern is found, the agent:
   - Determines the appropriate world (directory) in the ontology
   - Writes a fact file with the correct frontmatter schema
   - Sets initial `confidence` based on evidence strength
   - Sets `sources: 1` (this is the first independent observation)
   - Adds `refs` pointing to the raw evidence
4. **Proposal** — The agent follows the Knomit learning lifecycle:
   - Creates a learning branch
   - Commits one fact per file
   - Opens a PR for review and merge

## Example

An agent monitoring Spotify listening history notices:

1. Alice played 50 rock songs in June (episodic event 201)
2. Alice attended a rock festival in July (episodic event 215)
3. Alice added 3 rock albums to library in August (episodic event 230)

The agent synthesizes: "Alice likes rock music" with confidence 0.85, and opens a learning moment with three commits — one for the preference fact, one for the festival attendance, one for the album purchases.

## Key Principle

Knomit does not dictate how agents think. It provides the container (fact files), the protocol (Git operations), and the structure (ontology). The intelligence — what to observe, when to synthesize, how to reason — belongs to the agent.

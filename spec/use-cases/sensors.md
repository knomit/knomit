# Use Case: Sensors (Data Ingestion)

## Summary

Sensors are lightweight, domain-specific data collectors that feed raw observations into an episodic store. They are external to Knomit — they have no reasoning capabilities and no direct interaction with the knowledge repository.

## Role in the System

Sensors sit at the bottom of the data pipeline:

```
Sensors → Episodic Store → Agent (synthesis) → Knomit (facts)
```

Their sole job is to observe and record. They do not interpret, filter, or synthesize.

## Examples

- **System telemetry scraper** — monitors CPU, memory, network metrics and logs them to an episodic database
- **Network traffic interceptor** — captures and logs API calls, proxy traffic, or webhook payloads
- **User interface hooks** — records user interactions, clicks, form submissions
- **Conversation logger** — captures chat messages, emails, or meeting transcripts
- **File watcher** — monitors filesystem changes in a target directory

## Connection to Knomit

Sensors never write to the Knomit repository. Instead:

1. Sensors write raw events to an episodic store (SQLite, log files, APIs, etc.)
2. An agent periodically reviews the episodic store, identifies patterns or significant events
3. The agent synthesizes facts and proposes them to Knomit via the standard learning lifecycle (branch, commit, PR)
4. The resulting fact files may reference the original sensor data via `refs` (e.g., `episodic://event_88`)

## Key Principle

Knomit is agnostic to data sources. A fact could originate from automated sensors, manual user input, another AI agent, or any other process. Knomit only cares about the fact itself and how it enters the repository.

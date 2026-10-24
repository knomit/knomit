# Draft facts: fix/web-job-stream-keepalive

1. (gotchas/web/sse) handleJobEvents (internal/edge/web/handlers_jobs.go) writes the shared `: keepalive` comment (httpx sseStream.Keepalive) every jobEventsKeepalive (30s) while a job stream is idle. Before this, a synthesis waiting on the model emitted nothing and an idle stream could be dropped by proxies. Refs: handlers_jobs.go, TestJobs_EventsKeepaliveOnAnIdleStream.
   Does NOT mean the helper owns the ticker: each SSE handler (events, repo events, client sessions, logs, jobs) still hand-rolls its own 30s time.Ticker around stream.Keepalive().
2. (conventions/testing) jobEventsKeepalive is an unexported package var so the test shortens it to 20ms (restored via t.Cleanup); no real-seconds sleep. Falsified by neutering the Keepalive write: the test times out with only the initial running frame.

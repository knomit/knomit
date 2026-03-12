// Seed pushes test facts into a running knomit server via the MCP API.
// Usage: go run ./tools/seed/ [base|distill|all] [http://host:port]
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync/atomic"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// --- JSON-RPC types ---

type jsonrpcRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int64       `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

type jsonrpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// --- seed data types ---

type seedFact struct {
	Path       string   `json:"path"`
	Title      string   `json:"title"`
	Body       string   `json:"body"`
	Domain     []string `json:"domain"`
	Confidence float64  `json:"confidence"`
	Sources    int      `json:"sources"`
	Entities   []string `json:"entities"`
	Refs       []string `json:"refs"`
}

type moment struct {
	name  string
	facts []seedFact
}

// --- seed data ---

var baseMoments = []moment{
	{
		name: "seed-people",
		facts: []seedFact{
			{Path: "know/people/alice/likes-rock", Domain: []string{"music"}, Confidence: 0.9, Sources: 1, Entities: []string{"alice"}, Refs: []string{}, Title: "Alice prefers rock music", Body: "Alice consistently chooses rock over other genres. Particularly fond of classic rock from the 70s."},
			{Path: "know/people/alice/python-dev", Domain: []string{"programming"}, Confidence: 0.95, Sources: 2, Entities: []string{"alice"}, Refs: []string{}, Title: "Alice is a Python developer", Body: "Alice primarily works in Python. Uses pytest for testing and prefers FastAPI for web services."},
			{Path: "know/people/bob/jazz-fan", Domain: []string{"music"}, Confidence: 0.85, Sources: 1, Entities: []string{"bob"}, Refs: []string{}, Title: "Bob enjoys jazz", Body: "Bob listens to jazz regularly, especially Miles Davis and John Coltrane."},
			{Path: "know/people/bob/rust-dev", Domain: []string{"programming"}, Confidence: 0.8, Sources: 1, Entities: []string{"bob"}, Refs: []string{}, Title: "Bob is learning Rust", Body: "Bob has been learning Rust for systems programming. Coming from a C++ background."},
			{Path: "know/people/carol/tea-preference", Domain: []string{"food"}, Confidence: 0.95, Sources: 3, Entities: []string{"carol"}, Refs: []string{}, Title: "Carol prefers green tea", Body: "Carol drinks green tea exclusively. Has tried many varieties and prefers Japanese sencha."},
		},
	},
	{
		name: "seed-projects",
		facts: []seedFact{
			{Path: "know/projects/webapp/stack", Domain: []string{"architecture", "web"}, Confidence: 0.9, Sources: 1, Entities: []string{"webapp"}, Refs: []string{}, Title: "Webapp uses React + FastAPI", Body: "The webapp project uses React 18 on the frontend with FastAPI backend. PostgreSQL for persistence."},
			{Path: "know/projects/webapp/deploy", Domain: []string{"devops", "web"}, Confidence: 0.85, Sources: 1, Entities: []string{"webapp"}, Refs: []string{}, Title: "Webapp deploys to AWS ECS", Body: "Production deployment uses AWS ECS Fargate with ALB. CI/CD via GitHub Actions."},
			{Path: "know/projects/webapp/auth", Domain: []string{"security", "web"}, Confidence: 0.9, Sources: 2, Entities: []string{"webapp"}, Refs: []string{}, Title: "Webapp uses OAuth2 + JWT", Body: "Authentication is OAuth2 with Google and GitHub providers. JWT tokens with 1-hour expiry. Refresh tokens stored in httpOnly cookies."},
			{Path: "know/projects/ml-pipeline/overview", Domain: []string{"ml", "data"}, Confidence: 0.8, Sources: 1, Entities: []string{"ml-pipeline"}, Refs: []string{}, Title: "ML pipeline processes sensor data", Body: "Ingests IoT sensor data, runs anomaly detection using isolation forests, and alerts via PagerDuty."},
			{Path: "know/projects/ml-pipeline/infra", Domain: []string{"devops", "ml"}, Confidence: 0.75, Sources: 1, Entities: []string{"ml-pipeline"}, Refs: []string{}, Title: "ML pipeline runs on Kubernetes", Body: "Deployed on EKS with Argo Workflows for orchestration. Model artifacts stored in S3."},
		},
	},
	{
		name: "seed-decisions",
		facts: []seedFact{
			{Path: "know/decisions/use-bun", Domain: []string{"tooling"}, Confidence: 0.95, Sources: 3, Entities: []string{"bun"}, Refs: []string{}, Title: "Use Bun instead of Node.js", Body: "Team decided to standardize on Bun for all new TypeScript projects. Faster startup, built-in test runner, native SQLite."},
			{Path: "know/decisions/no-orm", Domain: []string{"architecture", "databases"}, Confidence: 0.85, Sources: 2, Entities: []string{}, Refs: []string{}, Title: "Prefer raw SQL over ORMs", Body: "After evaluating Prisma and Drizzle, the team decided to use raw SQL with type-safe query builders. ORMs add too much abstraction for our use cases."},
			{Path: "know/decisions/monorepo", Domain: []string{"architecture", "tooling"}, Confidence: 0.9, Sources: 2, Entities: []string{}, Refs: []string{}, Title: "Use monorepo structure", Body: "All related services live in a single monorepo. Turborepo for build orchestration. Shared packages in packages/ directory."},
			{Path: "know/decisions/testing-strategy", Domain: []string{"testing"}, Confidence: 0.8, Sources: 1, Entities: []string{}, Refs: []string{}, Title: "Integration tests over unit tests", Body: "Team prefers integration tests that exercise real database and API calls. Unit tests only for pure business logic with complex branching."},
			{Path: "know/decisions/api-versioning", Domain: []string{"architecture", "web"}, Confidence: 0.7, Sources: 1, Entities: []string{}, Refs: []string{}, Title: "URL-based API versioning", Body: "APIs versioned via URL path (/v1/, /v2/). Header-based versioning considered but rejected for simplicity."},
		},
	},
	{
		name: "seed-debugging",
		facts: []seedFact{
			{Path: "know/debugging/postgres-connection-pool", Domain: []string{"databases", "debugging"}, Confidence: 0.9, Sources: 2, Entities: []string{"postgres"}, Refs: []string{}, Title: "PostgreSQL connection pool exhaustion fix", Body: "Connection pool was exhausting under load because transactions weren't being released on error paths. Fixed by wrapping all DB calls in try/finally to ensure connection.release()."},
			{Path: "know/debugging/react-hydration-mismatch", Domain: []string{"web", "debugging"}, Confidence: 0.85, Sources: 1, Entities: []string{"react"}, Refs: []string{}, Title: "React hydration mismatch from date formatting", Body: "Server rendered dates in UTC but client formatted in local timezone, causing hydration mismatch. Fixed by using suppressHydrationWarning on date elements and formatting consistently."},
			{Path: "know/debugging/docker-layer-caching", Domain: []string{"devops", "debugging"}, Confidence: 0.8, Sources: 1, Entities: []string{"docker"}, Refs: []string{}, Title: "Docker build slow due to broken layer caching", Body: "COPY package.json was invalidating cache because it included the lock file. Split into two COPY steps: first package.json + lockfile, then source code."},
		},
	},
	{
		name: "seed-conventions",
		facts: []seedFact{
			{Path: "know/conventions/commit-messages", Domain: []string{"workflow"}, Confidence: 0.95, Sources: 3, Entities: []string{}, Refs: []string{}, Title: "Conventional commits format", Body: "All commits follow conventional commits: feat:, fix:, refactor:, docs:, test:, chore:. Scope is optional but encouraged."},
			{Path: "know/conventions/error-handling", Domain: []string{"programming"}, Confidence: 0.85, Sources: 2, Entities: []string{}, Refs: []string{}, Title: "Use Result types for expected errors", Body: "Expected errors (validation, not found) return Result types. Unexpected errors (network, disk) throw exceptions. Never use try/catch for control flow."},
		},
	},
}

var distillMoments = []moment{
	{
		name: "seed-distill-same-path",
		facts: []seedFact{
			{Path: "know/projects/auth-service/token-rotation", Domain: []string{"security", "architecture"}, Confidence: 0.9, Sources: 2, Entities: []string{"auth-service"}, Refs: []string{}, Title: "Auth service rotates JWT signing keys weekly", Body: "The auth service rotates its RSA signing keys every 7 days. Old keys remain valid for 24h after rotation. Key metadata is stored in Redis."},
			{Path: "know/projects/auth-service/rate-limiting", Domain: []string{"security", "architecture"}, Confidence: 0.85, Sources: 1, Entities: []string{"auth-service"}, Refs: []string{}, Title: "Auth service rate-limits login attempts", Body: "Login endpoints are rate-limited to 5 attempts per minute per IP. After 20 failures in an hour, the IP is blocked for 24 hours. Uses a sliding window counter in Redis."},
			{Path: "know/projects/auth-service/session-storage", Domain: []string{"security", "architecture"}, Confidence: 0.9, Sources: 2, Entities: []string{"auth-service"}, Refs: []string{}, Title: "Auth service stores sessions in Redis", Body: "User sessions are stored in Redis with a 30-minute sliding expiry. Session data includes user ID, roles, and last-active timestamp. Failover uses Redis Sentinel."},
			{Path: "know/projects/auth-service/mfa-flow", Domain: []string{"security"}, Confidence: 0.8, Sources: 1, Entities: []string{"auth-service"}, Refs: []string{}, Title: "Auth service supports TOTP-based MFA", Body: "Multi-factor authentication uses TOTP (RFC 6238) with 30-second windows. Recovery codes are generated at enrollment time and stored bcrypt-hashed."},
		},
	},
	{
		name: "seed-distill-cross-path-same-domain",
		facts: []seedFact{
			{Path: "know/people/dave/postgres-expert", Domain: []string{"databases"}, Confidence: 0.9, Sources: 2, Entities: []string{"dave", "postgres"}, Refs: []string{}, Title: "Dave is the team's PostgreSQL expert", Body: "Dave has 8 years of PostgreSQL experience. He reviews all schema migrations and handles performance tuning. Wrote the team's indexing guidelines."},
			{Path: "know/conventions/db-migrations", Domain: []string{"databases", "workflow"}, Confidence: 0.9, Sources: 3, Entities: []string{"postgres"}, Refs: []string{}, Title: "Database migrations use incremental SQL files", Body: "Schema changes use numbered .sql files in migrations/. Each migration is reviewed by Dave. Rollback scripts are mandatory. Never use auto-generated migrations."},
			{Path: "know/conventions/db-indexing", Domain: []string{"databases"}, Confidence: 0.85, Sources: 2, Entities: []string{"postgres"}, Refs: []string{}, Title: "Indexing policy for PostgreSQL tables", Body: "All foreign keys must have indexes. Composite indexes follow the left-prefix rule. Partial indexes preferred for boolean flags. GIN indexes for JSONB columns."},
			{Path: "know/debugging/postgres-vacuum-bloat", Domain: []string{"databases", "debugging"}, Confidence: 0.8, Sources: 1, Entities: []string{"postgres"}, Refs: []string{}, Title: "Table bloat from missed autovacuum", Body: "The orders table grew to 40GB (10GB actual data) because autovacuum was blocked by long-running analytics queries. Fixed by setting statement_timeout on the analytics role and running manual VACUUM FULL."},
		},
	},
	{
		name: "seed-distill-cross-path-shared-entities",
		facts: []seedFact{
			{Path: "know/decisions/container-runtime", Domain: []string{"devops"}, Confidence: 0.9, Sources: 2, Entities: []string{"kubernetes", "docker"}, Refs: []string{}, Title: "Standardized on containerd over Docker runtime", Body: "Kubernetes clusters migrated from Docker to containerd as the container runtime. Reduces overhead and aligns with upstream deprecation of dockershim."},
			{Path: "know/debugging/k8s-oom-kills", Domain: []string{"debugging"}, Confidence: 0.85, Sources: 2, Entities: []string{"kubernetes"}, Refs: []string{}, Title: "OOM kills from missing memory limits", Body: "Pods without memory limits were getting OOM-killed when nodes ran low. Fixed by enforcing LimitRange in all namespaces and adding resource requests/limits to all deployments."},
			{Path: "know/conventions/docker-images", Domain: []string{"devops", "security"}, Confidence: 0.9, Sources: 3, Entities: []string{"docker"}, Refs: []string{}, Title: "Docker images use distroless base", Body: "All production Docker images use Google's distroless base images. No shell, no package manager. Debug images with shell available in staging only."},
			{Path: "know/projects/platform/k8s-networking", Domain: []string{"networking"}, Confidence: 0.8, Sources: 1, Entities: []string{"kubernetes"}, Refs: []string{}, Title: "Kubernetes uses Cilium for networking", Body: "Cluster networking uses Cilium with eBPF for pod-to-pod communication. Network policies enforce namespace isolation. Hubble provides observability."},
		},
	},
	{
		name: "seed-distill-fca-split",
		facts: []seedFact{
			{Path: "know/projects/platform/prometheus-setup", Domain: []string{"observability"}, Confidence: 0.9, Sources: 2, Entities: []string{"prometheus"}, Refs: []string{}, Title: "Prometheus scrapes all services every 15s", Body: "Prometheus is configured with 15-second scrape intervals. Service discovery via Kubernetes annotations. Retention is 30 days. Thanos for long-term storage."},
			{Path: "know/conventions/metrics-naming", Domain: []string{"observability"}, Confidence: 0.85, Sources: 2, Entities: []string{"prometheus"}, Refs: []string{}, Title: "Prometheus metrics follow naming conventions", Body: "Metrics use the format <service>_<subsystem>_<name>_<unit>. Histograms for latencies, counters for requests, gauges for queue depth. Labels must be low-cardinality."},
			{Path: "know/debugging/prometheus-cardinality", Domain: []string{"observability", "debugging"}, Confidence: 0.8, Sources: 1, Entities: []string{"prometheus"}, Refs: []string{}, Title: "High cardinality labels crashed Prometheus", Body: "Adding user_id as a label on HTTP metrics caused Prometheus to OOM. Cardinality went from 200 to 2M time series. Fixed by removing the label and using exemplars instead."},
			{Path: "know/projects/platform/sentry-config", Domain: []string{"observability"}, Confidence: 0.75, Sources: 1, Entities: []string{"sentry"}, Refs: []string{}, Title: "Sentry captures frontend errors with 10% sampling", Body: "Sentry is configured with a 10% sample rate for performance transactions. Error events are always captured. Source maps uploaded during CI build step."},
		},
	},
	{
		name: "seed-distill-orphan",
		facts: []seedFact{
			{Path: "know/people/eve/sourdough-baker", Domain: []string{"food", "hobbies"}, Confidence: 0.7, Sources: 1, Entities: []string{"eve"}, Refs: []string{}, Title: "Eve bakes sourdough on weekends", Body: "Eve maintains a 3-year-old sourdough starter named 'Doughbert'. She bakes every Saturday morning and brings loaves to the office on Monday."},
		},
	},
}

// --- MCP client ---

var reqID atomic.Int64

func mcpCall(baseURL, sessionID, method string, params interface{}) (*jsonrpcResponse, string, error) {
	body := jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      reqID.Add(1),
		Method:  method,
		Params:  params,
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, sessionID, fmt.Errorf("marshal: %w", err)
	}

	req, err := http.NewRequest("POST", baseURL+"/mcp", bytes.NewReader(data))
	if err != nil {
		return nil, sessionID, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, sessionID, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	// Capture session ID from response.
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		sessionID = sid
	}

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, sessionID, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, sessionID, fmt.Errorf("read body: %w", err)
	}

	var result jsonrpcResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, sessionID, fmt.Errorf("unmarshal response: %w (body: %s)", err, string(respBody))
	}
	return &result, sessionID, nil
}

func main() {
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})

	phase := "all"
	baseURL := "http://localhost:3000"

	if len(os.Args) > 1 {
		phase = os.Args[1]
	}
	if len(os.Args) > 2 {
		baseURL = os.Args[2]
	}
	if phase != "base" && phase != "distill" && phase != "all" {
		fmt.Fprintf(os.Stderr, "unknown phase %q — use: base, distill, or all\n", phase)
		os.Exit(1)
	}

	// 1. Initialize MCP session.
	initParams := map[string]interface{}{
		"protocolVersion": "2025-03-26",
		"capabilities":    map[string]interface{}{},
		"clientInfo": map[string]interface{}{
			"name":    "knomit-seed",
			"version": "1.0.0",
		},
	}
	resp, sessionID, err := mcpCall(baseURL, "", "initialize", initParams)
	if err != nil {
		log.Fatal().Err(err).Msg("MCP initialize failed")
	}
	if resp.Error != nil {
		log.Fatal().Int("code", resp.Error.Code).Str("msg", resp.Error.Message).Msg("MCP initialize error")
	}
	log.Info().Str("session", sessionID).Msg("MCP session initialized")

	// 2. Send initialized notification.
	_, sessionID, err = mcpCall(baseURL, sessionID, "notifications/initialized", nil)
	if err != nil {
		log.Warn().Err(err).Msg("notifications/initialized failed (non-fatal)")
	}

	// 3. Select moments.
	var moments []moment
	switch phase {
	case "base":
		moments = baseMoments
	case "distill":
		moments = distillMoments
	default:
		moments = append(baseMoments, distillMoments...)
	}

	// 4. Call knomit_learn for each moment.
	totalFacts := 0
	for _, m := range moments {
		callParams := map[string]interface{}{
			"name": "knomit_learn",
			"arguments": map[string]interface{}{
				"moment_name": m.name,
				"facts":       m.facts,
			},
		}
		resp, sessionID, err = mcpCall(baseURL, sessionID, "tools/call", callParams)
		if err != nil {
			log.Fatal().Err(err).Str("moment", m.name).Msg("tools/call failed")
		}
		if resp.Error != nil {
			log.Fatal().Int("code", resp.Error.Code).Str("msg", resp.Error.Message).Str("moment", m.name).Msg("knomit_learn error")
		}
		log.Info().Str("moment", m.name).Int("facts", len(m.facts)).RawJSON("result", resp.Result).Msg("seeded")
		totalFacts += len(m.facts)
	}

	fmt.Printf("Seeded %d facts across %d moments (phase: %s).\n", totalFacts, len(moments), phase)
}

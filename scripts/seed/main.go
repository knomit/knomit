// Seed writes test facts directly into the knomit git store and search index.
// Build and run with: CGO_ENABLED=1 go run -tags sqlite_fts5 ./scripts/seed/
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"knomit/internal/config"
	"knomit/internal/git"
	"knomit/internal/mcp"
	"knomit/internal/store"
)

type seedFact struct {
	path       string
	title      string
	body       string
	domain     []string
	confidence float64
	sources    int
	entities   []string
}

type moment struct {
	name  string
	facts []seedFact
}

var baseMoments = []moment{
	{
		name: "seed-people",
		facts: []seedFact{
			{path: "know/people/alice/likes-rock", domain: []string{"music"}, confidence: 0.9, sources: 1, entities: []string{"alice"}, title: "Alice prefers rock music", body: "Alice consistently chooses rock over other genres. Particularly fond of classic rock from the 70s."},
			{path: "know/people/alice/python-dev", domain: []string{"programming"}, confidence: 0.95, sources: 2, entities: []string{"alice"}, title: "Alice is a Python developer", body: "Alice primarily works in Python. Uses pytest for testing and prefers FastAPI for web services."},
			{path: "know/people/bob/jazz-fan", domain: []string{"music"}, confidence: 0.85, sources: 1, entities: []string{"bob"}, title: "Bob enjoys jazz", body: "Bob listens to jazz regularly, especially Miles Davis and John Coltrane."},
			{path: "know/people/bob/rust-dev", domain: []string{"programming"}, confidence: 0.8, sources: 1, entities: []string{"bob"}, title: "Bob is learning Rust", body: "Bob has been learning Rust for systems programming. Coming from a C++ background."},
			{path: "know/people/carol/tea-preference", domain: []string{"food"}, confidence: 0.95, sources: 3, entities: []string{"carol"}, title: "Carol prefers green tea", body: "Carol drinks green tea exclusively. Has tried many varieties and prefers Japanese sencha."},
		},
	},
	{
		name: "seed-projects",
		facts: []seedFact{
			{path: "know/projects/webapp/stack", domain: []string{"architecture", "web"}, confidence: 0.9, sources: 1, entities: []string{"webapp"}, title: "Webapp uses React + FastAPI", body: "The webapp project uses React 18 on the frontend with FastAPI backend. PostgreSQL for persistence."},
			{path: "know/projects/webapp/deploy", domain: []string{"devops", "web"}, confidence: 0.85, sources: 1, entities: []string{"webapp"}, title: "Webapp deploys to AWS ECS", body: "Production deployment uses AWS ECS Fargate with ALB. CI/CD via GitHub Actions."},
			{path: "know/projects/webapp/auth", domain: []string{"security", "web"}, confidence: 0.9, sources: 2, entities: []string{"webapp"}, title: "Webapp uses OAuth2 + JWT", body: "Authentication is OAuth2 with Google and GitHub providers. JWT tokens with 1-hour expiry. Refresh tokens stored in httpOnly cookies."},
			{path: "know/projects/ml-pipeline/overview", domain: []string{"ml", "data"}, confidence: 0.8, sources: 1, entities: []string{"ml-pipeline"}, title: "ML pipeline processes sensor data", body: "Ingests IoT sensor data, runs anomaly detection using isolation forests, and alerts via PagerDuty."},
			{path: "know/projects/ml-pipeline/infra", domain: []string{"devops", "ml"}, confidence: 0.75, sources: 1, entities: []string{"ml-pipeline"}, title: "ML pipeline runs on Kubernetes", body: "Deployed on EKS with Argo Workflows for orchestration. Model artifacts stored in S3."},
		},
	},
	{
		name: "seed-decisions",
		facts: []seedFact{
			{path: "know/decisions/use-bun", domain: []string{"tooling"}, confidence: 0.95, sources: 3, entities: []string{"bun"}, title: "Use Bun instead of Node.js", body: "Team decided to standardize on Bun for all new TypeScript projects. Faster startup, built-in test runner, native SQLite."},
			{path: "know/decisions/no-orm", domain: []string{"architecture", "databases"}, confidence: 0.85, sources: 2, entities: []string{}, title: "Prefer raw SQL over ORMs", body: "After evaluating Prisma and Drizzle, the team decided to use raw SQL with type-safe query builders. ORMs add too much abstraction for our use cases."},
			{path: "know/decisions/monorepo", domain: []string{"architecture", "tooling"}, confidence: 0.9, sources: 2, entities: []string{}, title: "Use monorepo structure", body: "All related services live in a single monorepo. Turborepo for build orchestration. Shared packages in packages/ directory."},
			{path: "know/decisions/testing-strategy", domain: []string{"testing"}, confidence: 0.8, sources: 1, entities: []string{}, title: "Integration tests over unit tests", body: "Team prefers integration tests that exercise real database and API calls. Unit tests only for pure business logic with complex branching."},
			{path: "know/decisions/api-versioning", domain: []string{"architecture", "web"}, confidence: 0.7, sources: 1, entities: []string{}, title: "URL-based API versioning", body: "APIs versioned via URL path (/v1/, /v2/). Header-based versioning considered but rejected for simplicity."},
		},
	},
	{
		name: "seed-debugging",
		facts: []seedFact{
			{path: "know/debugging/postgres-connection-pool", domain: []string{"databases", "debugging"}, confidence: 0.9, sources: 2, entities: []string{"postgres"}, title: "PostgreSQL connection pool exhaustion fix", body: "Connection pool was exhausting under load because transactions weren't being released on error paths. Fixed by wrapping all DB calls in try/finally to ensure connection.release()."},
			{path: "know/debugging/react-hydration-mismatch", domain: []string{"web", "debugging"}, confidence: 0.85, sources: 1, entities: []string{"react"}, title: "React hydration mismatch from date formatting", body: "Server rendered dates in UTC but client formatted in local timezone, causing hydration mismatch. Fixed by using suppressHydrationWarning on date elements and formatting consistently."},
			{path: "know/debugging/docker-layer-caching", domain: []string{"devops", "debugging"}, confidence: 0.8, sources: 1, entities: []string{"docker"}, title: "Docker build slow due to broken layer caching", body: "COPY package.json was invalidating cache because it included the lock file. Split into two COPY steps: first package.json + lockfile, then source code."},
		},
	},
	{
		name: "seed-conventions",
		facts: []seedFact{
			{path: "know/conventions/commit-messages", domain: []string{"workflow"}, confidence: 0.95, sources: 3, entities: []string{}, title: "Conventional commits format", body: "All commits follow conventional commits: feat:, fix:, refactor:, docs:, test:, chore:. Scope is optional but encouraged."},
			{path: "know/conventions/error-handling", domain: []string{"programming"}, confidence: 0.85, sources: 2, entities: []string{}, title: "Use Result types for expected errors", body: "Expected errors (validation, not found) return Result types. Unexpected errors (network, disk) throw exceptions. Never use try/catch for control flow."},
		},
	},
}

var distillMoments = []moment{
	{
		name: "seed-distill-same-path",
		facts: []seedFact{
			{path: "know/projects/auth-service/token-rotation", domain: []string{"security", "architecture"}, confidence: 0.9, sources: 2, entities: []string{"auth-service"}, title: "Auth service rotates JWT signing keys weekly", body: "The auth service rotates its RSA signing keys every 7 days. Old keys remain valid for 24h after rotation. Key metadata is stored in Redis."},
			{path: "know/projects/auth-service/rate-limiting", domain: []string{"security", "architecture"}, confidence: 0.85, sources: 1, entities: []string{"auth-service"}, title: "Auth service rate-limits login attempts", body: "Login endpoints are rate-limited to 5 attempts per minute per IP. After 20 failures in an hour, the IP is blocked for 24 hours. Uses a sliding window counter in Redis."},
			{path: "know/projects/auth-service/session-storage", domain: []string{"security", "architecture"}, confidence: 0.9, sources: 2, entities: []string{"auth-service"}, title: "Auth service stores sessions in Redis", body: "User sessions are stored in Redis with a 30-minute sliding expiry. Session data includes user ID, roles, and last-active timestamp. Failover uses Redis Sentinel."},
			{path: "know/projects/auth-service/mfa-flow", domain: []string{"security"}, confidence: 0.8, sources: 1, entities: []string{"auth-service"}, title: "Auth service supports TOTP-based MFA", body: "Multi-factor authentication uses TOTP (RFC 6238) with 30-second windows. Recovery codes are generated at enrollment time and stored bcrypt-hashed."},
		},
	},
	{
		name: "seed-distill-cross-path-same-domain",
		facts: []seedFact{
			{path: "know/people/dave/postgres-expert", domain: []string{"databases"}, confidence: 0.9, sources: 2, entities: []string{"dave", "postgres"}, title: "Dave is the team's PostgreSQL expert", body: "Dave has 8 years of PostgreSQL experience. He reviews all schema migrations and handles performance tuning. Wrote the team's indexing guidelines."},
			{path: "know/conventions/db-migrations", domain: []string{"databases", "workflow"}, confidence: 0.9, sources: 3, entities: []string{"postgres"}, title: "Database migrations use incremental SQL files", body: "Schema changes use numbered .sql files in migrations/. Each migration is reviewed by Dave. Rollback scripts are mandatory. Never use auto-generated migrations."},
			{path: "know/conventions/db-indexing", domain: []string{"databases"}, confidence: 0.85, sources: 2, entities: []string{"postgres"}, title: "Indexing policy for PostgreSQL tables", body: "All foreign keys must have indexes. Composite indexes follow the left-prefix rule. Partial indexes preferred for boolean flags. GIN indexes for JSONB columns."},
			{path: "know/debugging/postgres-vacuum-bloat", domain: []string{"databases", "debugging"}, confidence: 0.8, sources: 1, entities: []string{"postgres"}, title: "Table bloat from missed autovacuum", body: "The orders table grew to 40GB (10GB actual data) because autovacuum was blocked by long-running analytics queries. Fixed by setting statement_timeout on the analytics role and running manual VACUUM FULL."},
		},
	},
	{
		name: "seed-distill-cross-path-shared-entities",
		facts: []seedFact{
			{path: "know/decisions/container-runtime", domain: []string{"devops"}, confidence: 0.9, sources: 2, entities: []string{"kubernetes", "docker"}, title: "Standardized on containerd over Docker runtime", body: "Kubernetes clusters migrated from Docker to containerd as the container runtime. Reduces overhead and aligns with upstream deprecation of dockershim."},
			{path: "know/debugging/k8s-oom-kills", domain: []string{"debugging"}, confidence: 0.85, sources: 2, entities: []string{"kubernetes"}, title: "OOM kills from missing memory limits", body: "Pods without memory limits were getting OOM-killed when nodes ran low. Fixed by enforcing LimitRange in all namespaces and adding resource requests/limits to all deployments."},
			{path: "know/conventions/docker-images", domain: []string{"devops", "security"}, confidence: 0.9, sources: 3, entities: []string{"docker"}, title: "Docker images use distroless base", body: "All production Docker images use Google's distroless base images. No shell, no package manager. Debug images with shell available in staging only."},
			{path: "know/projects/platform/k8s-networking", domain: []string{"networking"}, confidence: 0.8, sources: 1, entities: []string{"kubernetes"}, title: "Kubernetes uses Cilium for networking", body: "Cluster networking uses Cilium with eBPF for pod-to-pod communication. Network policies enforce namespace isolation. Hubble provides observability."},
		},
	},
	{
		name: "seed-distill-fca-split",
		facts: []seedFact{
			{path: "know/projects/platform/prometheus-setup", domain: []string{"observability"}, confidence: 0.9, sources: 2, entities: []string{"prometheus"}, title: "Prometheus scrapes all services every 15s", body: "Prometheus is configured with 15-second scrape intervals. Service discovery via Kubernetes annotations. Retention is 30 days. Thanos for long-term storage."},
			{path: "know/conventions/metrics-naming", domain: []string{"observability"}, confidence: 0.85, sources: 2, entities: []string{"prometheus"}, title: "Prometheus metrics follow naming conventions", body: "Metrics use the format <service>_<subsystem>_<name>_<unit>. Histograms for latencies, counters for requests, gauges for queue depth. Labels must be low-cardinality."},
			{path: "know/debugging/prometheus-cardinality", domain: []string{"observability", "debugging"}, confidence: 0.8, sources: 1, entities: []string{"prometheus"}, title: "High cardinality labels crashed Prometheus", body: "Adding user_id as a label on HTTP metrics caused Prometheus to OOM. Cardinality went from 200 to 2M time series. Fixed by removing the label and using exemplars instead."},
			{path: "know/projects/platform/sentry-config", domain: []string{"observability"}, confidence: 0.75, sources: 1, entities: []string{"sentry"}, title: "Sentry captures frontend errors with 10% sampling", body: "Sentry is configured with a 10% sample rate for performance transactions. Error events are always captured. Source maps uploaded during CI build step."},
		},
	},
	{
		name: "seed-distill-orphan",
		facts: []seedFact{
			{path: "know/people/eve/sourdough-baker", domain: []string{"food", "hobbies"}, confidence: 0.7, sources: 1, entities: []string{"eve"}, title: "Eve bakes sourdough on weekends", body: "Eve maintains a 3-year-old sourdough starter named 'Doughbert'. She bakes every Saturday morning and brings loaves to the office on Monday."},
		},
	},
}

func main() {
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})

	phase := "all"
	if len(os.Args) > 1 {
		phase = os.Args[1]
	}
	if phase != "base" && phase != "distill" && phase != "all" {
		fmt.Fprintf(os.Stderr, "unknown phase %q — use: base, distill, or all\n", phase)
		os.Exit(1)
	}

	cfg := config.FromEnv()
	gitDBPath := filepath.Join(cfg.RepoPath, "knomit.git.db")
	idxDBPath := filepath.Join(cfg.RepoPath, "knomit.index.db")

	// Open or init git store.
	gs, err := git.Open(gitDBPath)
	if err != nil {
		log.Info().Str("path", gitDBPath).Msg("git store not found, initialising")
		gs, err = git.Init(gitDBPath)
		if err != nil {
			log.Fatal().Err(err).Msg("git.Init failed")
		}
	}
	defer gs.Close()

	// Open search index.
	idx, err := store.New(idxDBPath)
	if err != nil {
		log.Fatal().Err(err).Msg("store.New failed")
	}
	defer idx.Close()

	// Select moments.
	var moments []moment
	switch phase {
	case "base":
		moments = baseMoments
	case "distill":
		moments = distillMoments
	default:
		moments = append(baseMoments, distillMoments...)
	}

	totalFacts := 0
	skipped := 0

	for _, m := range moments {
		// Check if the first fact already exists.
		probePath := m.facts[0].path + ".md"
		exists, err := gs.FileExists(probePath)
		if err != nil {
			log.Fatal().Err(err).Str("moment", m.name).Msg("FileExists failed")
		}
		if exists {
			log.Info().Str("moment", m.name).Msg("already seeded, skipping")
			skipped++
			continue
		}

		// Build the files map and FactRecords for batch write + upsert.
		files := make(map[string]string, len(m.facts))
		records := make([]store.FactRecord, 0, len(m.facts))

		for _, sf := range m.facts {
			f := mcp.Fact{
				Path:       sf.path,
				Title:      sf.title,
				Body:       sf.body,
				Domain:     sf.domain,
				Confidence: sf.confidence,
				Sources:    sf.sources,
				Entities:   sf.entities,
				Refs:       []string{},
			}
			content := mcp.SerializeFact(f)
			filePath := sf.path + ".md"
			files[filePath] = content

			records = append(records, store.FactRecord{
				Path:       filePath,
				Title:      sf.title,
				Body:       sf.body,
				Domain:     sf.domain,
				Entities:   sf.entities,
				Confidence: sf.confidence,
				Sources:    sf.sources,
				Refs:       []string{},
			})
		}

		// Write all facts in one commit.
		if err := gs.BatchWrite(files, "seed: "+m.name); err != nil {
			log.Fatal().Err(err).Str("moment", m.name).Msg("BatchWrite failed")
		}

		// Tag the moment.
		if err := gs.Tag("learn/" + m.name); err != nil {
			log.Fatal().Err(err).Str("moment", m.name).Msg("Tag failed")
		}

		// Get the HEAD commit hash for the index records.
		headHash, err := gs.HeadCommit()
		if err != nil {
			log.Fatal().Err(err).Str("moment", m.name).Msg("HeadCommit failed")
		}

		// Upsert each fact into the search index.
		for i := range records {
			records[i].CommitHash = headHash
			if err := idx.Upsert(records[i]); err != nil {
				log.Fatal().Err(err).Str("path", records[i].Path).Msg("Upsert failed")
			}
		}

		log.Info().Str("moment", m.name).Int("facts", len(m.facts)).Msg("seeded")
		totalFacts += len(m.facts)
	}

	if skipped > 0 {
		fmt.Printf("Seeded %d facts, skipped %d already-seeded moments (phase: %s).\n", totalFacts, skipped, phase)
	} else {
		fmt.Printf("Seeded %d facts (phase: %s).\n", totalFacts, phase)
	}
}

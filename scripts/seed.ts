#!/usr/bin/env bun
/**
 * Seed the knowledge base with sample facts for testing.
 *
 * Usage:
 *   bun scripts/seed.ts [phase] [repo-path]
 *
 * Phases:
 *   base       Seed original facts (people, projects, decisions, debugging, conventions)
 *   distill    Seed distillation-focused facts (clustering test cases)
 *   all        Seed everything at once (default)
 *
 * Workflow:
 *   bun scripts/seed.ts base          # step 1: seed base facts
 *   bun index.ts synthesize           # step 2: run first synthesis
 *   bun scripts/seed.ts distill       # step 3: add distillation test facts
 *   bun index.ts synthesize           # step 4: run second synthesis (exercises clustering)
 */
import { GitRepo } from "../src/git";
import { learnHandler } from "../src/tools/learn";

const phase = process.argv[2] ?? "all";
const repoPath = process.argv[3] ?? `${process.env.HOME}/.knomit`;

if (!["base", "distill", "all"].includes(phase)) {
  console.error(`Unknown phase "${phase}". Use: base, distill, or all`);
  process.exit(1);
}

const repo = new GitRepo(repoPath);
await repo.init();

// --- Base facts ---
const baseMoments = [
  {
    moment_name: "seed-people",
    facts: [
      { path: "worlds/people/alice/likes-rock", domain: ["music"], confidence: 0.9, sources: 1, entities: ["alice"], title: "Alice prefers rock music", body: "Alice consistently chooses rock over other genres. Particularly fond of classic rock from the 70s." },
      { path: "worlds/people/alice/python-dev", domain: ["programming"], confidence: 0.95, sources: 2, entities: ["alice"], title: "Alice is a Python developer", body: "Alice primarily works in Python. Uses pytest for testing and prefers FastAPI for web services." },
      { path: "worlds/people/bob/jazz-fan", domain: ["music"], confidence: 0.85, sources: 1, entities: ["bob"], title: "Bob enjoys jazz", body: "Bob listens to jazz regularly, especially Miles Davis and John Coltrane." },
      { path: "worlds/people/bob/rust-dev", domain: ["programming"], confidence: 0.8, sources: 1, entities: ["bob"], title: "Bob is learning Rust", body: "Bob has been learning Rust for systems programming. Coming from a C++ background." },
      { path: "worlds/people/carol/tea-preference", domain: ["food"], confidence: 0.95, sources: 3, entities: ["carol"], title: "Carol prefers green tea", body: "Carol drinks green tea exclusively. Has tried many varieties and prefers Japanese sencha." },
    ],
  },
  {
    moment_name: "seed-projects",
    facts: [
      { path: "worlds/projects/webapp/stack", domain: ["architecture", "web"], confidence: 0.9, sources: 1, entities: ["webapp"], title: "Webapp uses React + FastAPI", body: "The webapp project uses React 18 on the frontend with FastAPI backend. PostgreSQL for persistence." },
      { path: "worlds/projects/webapp/deploy", domain: ["devops", "web"], confidence: 0.85, sources: 1, entities: ["webapp"], title: "Webapp deploys to AWS ECS", body: "Production deployment uses AWS ECS Fargate with ALB. CI/CD via GitHub Actions." },
      { path: "worlds/projects/webapp/auth", domain: ["security", "web"], confidence: 0.9, sources: 2, entities: ["webapp"], title: "Webapp uses OAuth2 + JWT", body: "Authentication is OAuth2 with Google and GitHub providers. JWT tokens with 1-hour expiry. Refresh tokens stored in httpOnly cookies." },
      { path: "worlds/projects/ml-pipeline/overview", domain: ["ml", "data"], confidence: 0.8, sources: 1, entities: ["ml-pipeline"], title: "ML pipeline processes sensor data", body: "Ingests IoT sensor data, runs anomaly detection using isolation forests, and alerts via PagerDuty." },
      { path: "worlds/projects/ml-pipeline/infra", domain: ["devops", "ml"], confidence: 0.75, sources: 1, entities: ["ml-pipeline"], title: "ML pipeline runs on Kubernetes", body: "Deployed on EKS with Argo Workflows for orchestration. Model artifacts stored in S3." },
    ],
  },
  {
    moment_name: "seed-decisions",
    facts: [
      { path: "worlds/decisions/use-bun", domain: ["tooling"], confidence: 0.95, sources: 3, entities: ["bun"], title: "Use Bun instead of Node.js", body: "Team decided to standardize on Bun for all new TypeScript projects. Faster startup, built-in test runner, native SQLite." },
      { path: "worlds/decisions/no-orm", domain: ["architecture", "databases"], confidence: 0.85, sources: 2, entities: [], title: "Prefer raw SQL over ORMs", body: "After evaluating Prisma and Drizzle, the team decided to use raw SQL with type-safe query builders. ORMs add too much abstraction for our use cases." },
      { path: "worlds/decisions/monorepo", domain: ["architecture", "tooling"], confidence: 0.9, sources: 2, entities: [], title: "Use monorepo structure", body: "All related services live in a single monorepo. Turborepo for build orchestration. Shared packages in packages/ directory." },
      { path: "worlds/decisions/testing-strategy", domain: ["testing"], confidence: 0.8, sources: 1, entities: [], title: "Integration tests over unit tests", body: "Team prefers integration tests that exercise real database and API calls. Unit tests only for pure business logic with complex branching." },
      { path: "worlds/decisions/api-versioning", domain: ["architecture", "web"], confidence: 0.7, sources: 1, entities: [], title: "URL-based API versioning", body: "APIs versioned via URL path (/v1/, /v2/). Header-based versioning considered but rejected for simplicity." },
    ],
  },
  {
    moment_name: "seed-debugging",
    facts: [
      { path: "worlds/debugging/postgres-connection-pool", domain: ["databases", "debugging"], confidence: 0.9, sources: 2, entities: ["postgres"], title: "PostgreSQL connection pool exhaustion fix", body: "Connection pool was exhausting under load because transactions weren't being released on error paths. Fixed by wrapping all DB calls in try/finally to ensure connection.release()." },
      { path: "worlds/debugging/react-hydration-mismatch", domain: ["web", "debugging"], confidence: 0.85, sources: 1, entities: ["react"], title: "React hydration mismatch from date formatting", body: "Server rendered dates in UTC but client formatted in local timezone, causing hydration mismatch. Fixed by using suppressHydrationWarning on date elements and formatting consistently." },
      { path: "worlds/debugging/docker-layer-caching", domain: ["devops", "debugging"], confidence: 0.8, sources: 1, entities: ["docker"], title: "Docker build slow due to broken layer caching", body: "COPY package.json was invalidating cache because it included the lock file. Split into two COPY steps: first package.json + lockfile, then source code." },
    ],
  },
  {
    moment_name: "seed-conventions",
    facts: [
      { path: "worlds/conventions/commit-messages", domain: ["workflow"], confidence: 0.95, sources: 3, entities: [], title: "Conventional commits format", body: "All commits follow conventional commits: feat:, fix:, refactor:, docs:, test:, chore:. Scope is optional but encouraged." },
      { path: "worlds/conventions/error-handling", domain: ["programming"], confidence: 0.85, sources: 2, entities: [], title: "Use Result types for expected errors", body: "Expected errors (validation, not found) return Result types. Unexpected errors (network, disk) throw exceptions. Never use try/catch for control flow." },
    ],
  },
];

// --- Distillation-focused facts ---
// Designed to exercise all clustering cases in the stratified distillation pipeline.
const distillMoments = [
  {
    // CASE 1: Same ontology path, same domain/entities — tight cluster.
    // Should cluster via embedding similarity AND FCA validation
    // (shared entity "auth-service", shared domains "security", "architecture").
    moment_name: "seed-distill-same-path",
    facts: [
      { path: "worlds/projects/auth-service/token-rotation", domain: ["security", "architecture"], confidence: 0.9, sources: 2, entities: ["auth-service"], title: "Auth service rotates JWT signing keys weekly", body: "The auth service rotates its RSA signing keys every 7 days. Old keys remain valid for 24h after rotation. Key metadata is stored in Redis." },
      { path: "worlds/projects/auth-service/rate-limiting", domain: ["security", "architecture"], confidence: 0.85, sources: 1, entities: ["auth-service"], title: "Auth service rate-limits login attempts", body: "Login endpoints are rate-limited to 5 attempts per minute per IP. After 20 failures in an hour, the IP is blocked for 24 hours. Uses a sliding window counter in Redis." },
      { path: "worlds/projects/auth-service/session-storage", domain: ["security", "architecture"], confidence: 0.9, sources: 2, entities: ["auth-service"], title: "Auth service stores sessions in Redis", body: "User sessions are stored in Redis with a 30-minute sliding expiry. Session data includes user ID, roles, and last-active timestamp. Failover uses Redis Sentinel." },
      { path: "worlds/projects/auth-service/mfa-flow", domain: ["security"], confidence: 0.8, sources: 1, entities: ["auth-service"], title: "Auth service supports TOTP-based MFA", body: "Multi-factor authentication uses TOTP (RFC 6238) with 30-second windows. Recovery codes are generated at enrollment time and stored bcrypt-hashed." },
    ],
  },
  {
    // CASE 2: Cross ontology paths, same domain — clusters via embedding similarity.
    // Lives under different paths (people/dave, conventions, debugging) but all
    // share domain "databases". FCA keeps them together via shared domain.
    moment_name: "seed-distill-cross-path-same-domain",
    facts: [
      { path: "worlds/people/dave/postgres-expert", domain: ["databases"], confidence: 0.9, sources: 2, entities: ["dave", "postgres"], title: "Dave is the team's PostgreSQL expert", body: "Dave has 8 years of PostgreSQL experience. He reviews all schema migrations and handles performance tuning. Wrote the team's indexing guidelines." },
      { path: "worlds/conventions/db-migrations", domain: ["databases", "workflow"], confidence: 0.9, sources: 3, entities: ["postgres"], title: "Database migrations use incremental SQL files", body: "Schema changes use numbered .sql files in migrations/. Each migration is reviewed by Dave. Rollback scripts are mandatory. Never use auto-generated migrations." },
      { path: "worlds/conventions/db-indexing", domain: ["databases"], confidence: 0.85, sources: 2, entities: ["postgres"], title: "Indexing policy for PostgreSQL tables", body: "All foreign keys must have indexes. Composite indexes follow the left-prefix rule. Partial indexes preferred for boolean flags. GIN indexes for JSONB columns." },
      { path: "worlds/debugging/postgres-vacuum-bloat", domain: ["databases", "debugging"], confidence: 0.8, sources: 1, entities: ["postgres"], title: "Table bloat from missed autovacuum", body: "The orders table grew to 40GB (10GB actual data) because autovacuum was blocked by long-running analytics queries. Fixed by setting statement_timeout on the analytics role and running manual VACUUM FULL." },
    ],
  },
  {
    // CASE 3: Cross ontology paths, shared entities (not domains).
    // Different paths and different domains, but share entities "kubernetes" and "docker".
    // FCA connects them via shared entities.
    moment_name: "seed-distill-cross-path-shared-entities",
    facts: [
      { path: "worlds/decisions/container-runtime", domain: ["devops"], confidence: 0.9, sources: 2, entities: ["kubernetes", "docker"], title: "Standardized on containerd over Docker runtime", body: "Kubernetes clusters migrated from Docker to containerd as the container runtime. Reduces overhead and aligns with upstream deprecation of dockershim." },
      { path: "worlds/debugging/k8s-oom-kills", domain: ["debugging"], confidence: 0.85, sources: 2, entities: ["kubernetes"], title: "OOM kills from missing memory limits", body: "Pods without memory limits were getting OOM-killed when nodes ran low. Fixed by enforcing LimitRange in all namespaces and adding resource requests/limits to all deployments." },
      { path: "worlds/conventions/docker-images", domain: ["devops", "security"], confidence: 0.9, sources: 3, entities: ["docker"], title: "Docker images use distroless base", body: "All production Docker images use Google's distroless base images. No shell, no package manager. Debug images with shell available in staging only." },
      { path: "worlds/projects/platform/k8s-networking", domain: ["networking"], confidence: 0.8, sources: 1, entities: ["kubernetes"], title: "Kubernetes uses Cilium for networking", body: "Cluster networking uses Cilium with eBPF for pod-to-pod communication. Network policies enforce namespace isolation. Hubble provides observability." },
    ],
  },
  {
    // CASE 4: Semantically similar but different metadata — tests FCA splitting.
    // All about "monitoring" semantically, but split into two FCA groups:
    //   Group A: entity "prometheus" (3 facts) — should form a cluster
    //   Group B: entity "sentry" (1 fact) — too small, should become noise
    moment_name: "seed-distill-fca-split",
    facts: [
      { path: "worlds/projects/platform/prometheus-setup", domain: ["observability"], confidence: 0.9, sources: 2, entities: ["prometheus"], title: "Prometheus scrapes all services every 15s", body: "Prometheus is configured with 15-second scrape intervals. Service discovery via Kubernetes annotations. Retention is 30 days. Thanos for long-term storage." },
      { path: "worlds/conventions/metrics-naming", domain: ["observability"], confidence: 0.85, sources: 2, entities: ["prometheus"], title: "Prometheus metrics follow naming conventions", body: "Metrics use the format <service>_<subsystem>_<name>_<unit>. Histograms for latencies, counters for requests, gauges for queue depth. Labels must be low-cardinality." },
      { path: "worlds/debugging/prometheus-cardinality", domain: ["observability", "debugging"], confidence: 0.8, sources: 1, entities: ["prometheus"], title: "High cardinality labels crashed Prometheus", body: "Adding user_id as a label on HTTP metrics caused Prometheus to OOM. Cardinality went from 200 to 2M time series. Fixed by removing the label and using exemplars instead." },
      { path: "worlds/projects/platform/sentry-config", domain: ["observability"], confidence: 0.75, sources: 1, entities: ["sentry"], title: "Sentry captures frontend errors with 10% sampling", body: "Sentry is configured with a 10% sample rate for performance transactions. Error events are always captured. Source maps uploaded during CI build step." },
    ],
  },
  {
    // CASE 5: Isolated fact — no cluster match.
    // Unique domain/entity combination. With min_cluster_size=3,
    // should end up as noise (not enough similar facts to cluster with).
    moment_name: "seed-distill-orphan",
    facts: [
      { path: "worlds/people/eve/sourdough-baker", domain: ["food", "hobbies"], confidence: 0.7, sources: 1, entities: ["eve"], title: "Eve bakes sourdough on weekends", body: "Eve maintains a 3-year-old sourdough starter named 'Doughbert'. She bakes every Saturday morning and brings loaves to the office on Monday." },
    ],
  },
];

// --- Select moments based on phase ---
const selectedMoments = phase === "base" ? baseMoments
  : phase === "distill" ? distillMoments
  : [...baseMoments, ...distillMoments];

let totalFacts = 0;
for (const moment of selectedMoments) {
  const result = await learnHandler(repo, {
    moment_name: moment.moment_name,
    facts: moment.facts.map((f) => ({ ...f, refs: [] })),
  });
  totalFacts += result.commits.length;
  console.log(`${moment.moment_name}: ${result.commits.length} facts, tag=${result.moment_tag}`);
}

console.log(`\nSeeded ${totalFacts} facts (phase: ${phase}).`);

// --- Print expectations ---
if (phase === "base" || phase === "all") {
  console.log(`
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
  STEP 1 EXPECTATIONS (bun index.ts synthesize)
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

PRUNE should:
  - Merge alice/likes-rock + bob/jazz-fan → people music overview (shared domain "music")
  - Merge alice/python-dev + bob/rust-dev → people programming overview (shared domain "programming")
  - Merge webapp/stack + webapp/deploy + webapp/auth → webapp overview (shared entity "webapp")
  - Merge ml-pipeline/overview + ml-pipeline/infra → ml-pipeline overview (shared entity "ml-pipeline")
  - Keep carol/tea-preference, decisions, debugging, conventions as-is

DISTILL should:
  - Cluster webapp-related facts → distill "webapp architecture and operations"
  - Cluster decisions (architecture-related) → distill "team architecture principles"
  - Leave small/isolated groups as noise
`);
}

if (phase === "distill" || phase === "all") {
  console.log(`
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
  ${phase === "distill" ? "STEP 2" : "DISTILL"} EXPECTATIONS (bun index.ts synthesize)
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

CASE 1 — Same ontology path, tight cluster (4 facts)
  Path: worlds/projects/auth-service/*
  Shared: domain=[security, architecture], entity=[auth-service]
  Expect: Cluster together. Distill into "auth-service security overview"
          covering token rotation, rate limiting, sessions, and MFA.

CASE 2 — Cross ontology paths, shared domain (4 facts)
  Paths: worlds/people/dave/*, worlds/conventions/db-*, worlds/debugging/postgres-*
  Shared: domain=[databases], entity=[postgres]
  Expect: Cluster via embedding similarity + FCA keeps them together (shared
          domain "databases" and entity "postgres"). Distill into
          "team database practices and PostgreSQL patterns".

CASE 3 — Cross ontology paths, shared entities (4 facts)
  Paths: worlds/decisions/*, worlds/debugging/*, worlds/conventions/*, worlds/projects/platform/*
  Shared: entities=[kubernetes, docker] (domains vary)
  Expect: Cluster via embedding similarity. FCA connects via shared entities
          "kubernetes" and "docker". Distill into "container infrastructure patterns".

CASE 4 — FCA split test (4 facts, but split into 3+1)
  Paths: worlds/projects/platform/prometheus-*, worlds/conventions/metrics-*,
         worlds/debugging/prometheus-*, worlds/projects/platform/sentry-*
  Shared: domain=[observability], but entities diverge (prometheus vs sentry)
  Expect: If embeddings cluster all 4 together, FCA should split into:
          - prometheus group (3 facts) → cluster, distill into "Prometheus patterns"
          - sentry group (1 fact) → noise (below min_cluster_size=3)

CASE 5 — Orphan fact (1 fact)
  Path: worlds/people/eve/sourdough-baker
  Shared: nothing relevant to other facts
  Expect: Noise. Not clustered. Left untouched by distillation.
`);
}

#!/usr/bin/env bun
/**
 * Seed the knowledge base with sample facts for testing.
 *
 * Usage:
 *   bun scripts/seed.ts              # seed default repo (~/.knomit)
 *   bun scripts/seed.ts /tmp/test    # seed a specific repo path
 */
import { GitRepo } from "../src/git";
import { learnHandler } from "../src/tools/learn";

const repoPath = process.argv[2] ?? `${process.env.HOME}/.knomit`;
const repo = new GitRepo(repoPath);
await repo.init();

const moments = [
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

let totalFacts = 0;
for (const moment of moments) {
  const result = await learnHandler(repo, {
    moment_name: moment.moment_name,
    facts: moment.facts.map((f) => ({ ...f, refs: [] })),
  });
  totalFacts += result.commits.length;
  console.log(`${moment.moment_name}: ${result.commits.length} facts, tag=${result.moment_tag}`);
}

console.log(`\nSeeded ${totalFacts} facts in ${moments.length} learning moments.`);

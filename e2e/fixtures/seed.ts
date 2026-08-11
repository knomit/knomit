/**
 * Seed data for e2e tests.
 *
 * Creates 20+ facts across 4 domains in 5 sequential batches,
 * with delays between batches to produce distinct commits for
 * history testing.
 */

interface SeedFact {
  path: string;
  content: string;
}

// ── Domain constants ────────────────────────────────────────────

export const SEED_DOMAINS = ['databases', 'networking', 'security', 'observability'] as const;

// ── Fact definitions ────────────────────────────────────────────

const facts: SeedFact[] = [
  // ── Batch 1: databases basics ─────────────────────────────────
  {
    path: 'kb/databases/postgresql/mvcc.md',
    content: `---
confidence: 0.9
type: observation
entities:
  - PostgreSQL
  - MVCC
---
# PostgreSQL MVCC

PostgreSQL uses Multi-Version Concurrency Control (MVCC) to handle concurrent access. Each transaction sees a snapshot of the database, allowing readers and writers to operate without blocking each other.`,
  },
  {
    path: 'kb/databases/postgresql/vacuum.md',
    content: `---
confidence: 0.85
type: principle
entities:
  - PostgreSQL
  - VACUUM
---
# PostgreSQL VACUUM

Regular VACUUM operations are essential for reclaiming storage from dead tuples. Autovacuum should be tuned based on table write patterns — high-churn tables benefit from more aggressive settings.`,
  },
  {
    path: 'kb/databases/indexing/btree-vs-hash.md',
    content: `---
confidence: 0.8
type: observation
entities:
  - B-tree index
  - Hash index
refs:
  - https://use-the-index-luke.com/
---
# B-tree vs Hash Indexes

B-tree indexes support range queries and ordering while hash indexes only support equality comparisons. In PostgreSQL, B-tree is the default and most versatile index type.`,
  },
  {
    path: 'kb/databases/redis/eviction-policies.md',
    content: `---
confidence: 0.7
type: observation
entities:
  - Redis
  - cache eviction
---
# Redis Eviction Policies

Redis supports multiple eviction policies including allkeys-lru, volatile-lru, and noeviction. Choosing the right policy depends on whether all keys are cache-like or some must persist.`,
  },

  {
    path: 'kb/databases/postgresql/query-planning.md',
    content: `---
confidence: 0.85
type: concept
entities:
  - PostgreSQL
  - query planner
refs:
  - kb/databases/postgresql/mvcc.md
  - kb/databases/indexing/btree-vs-hash.md
---
# PostgreSQL Query Planning

The query planner evaluates multiple execution strategies and picks the one with the lowest estimated cost. Understanding MVCC and index types is essential for predicting planner behavior.`,
  },

  // ── Batch 2: networking ───────────────────────────────────────
  {
    path: 'kb/networking/dns/resolution-flow.md',
    content: `---
confidence: 0.95
type: observation
entities:
  - DNS
  - recursive resolver
---
# DNS Resolution Flow

DNS resolution follows a hierarchical lookup: stub resolver -> recursive resolver -> root nameserver -> TLD nameserver -> authoritative nameserver. Caching at each level reduces latency.`,
  },
  {
    path: 'kb/networking/tcp/congestion-control.md',
    content: `---
confidence: 0.8
type: observation
entities:
  - TCP
  - congestion control
  - BBR
refs:
  - https://datatracker.ietf.org/doc/html/rfc5681
---
# TCP Congestion Control

Modern TCP stacks use algorithms like BBR (Bottleneck Bandwidth and RTT) instead of traditional loss-based approaches. BBR models the network path to maximize throughput without filling buffers.`,
  },
  {
    path: 'kb/networking/http/http3-quic.md',
    content: `---
confidence: 0.7
type: observation
entities:
  - HTTP/3
  - QUIC
  - UDP
---
# HTTP/3 and QUIC

HTTP/3 runs over QUIC (UDP-based) rather than TCP. This eliminates head-of-line blocking at the transport layer and enables 0-RTT connection resumption.`,
  },
  {
    path: 'kb/networking/load-balancing/l4-vs-l7.md',
    content: `---
confidence: 0.85
type: principle
entities:
  - load balancer
  - L4
  - L7
---
# L4 vs L7 Load Balancing

L4 (transport) load balancers route based on IP/port and are faster but less flexible. L7 (application) load balancers can inspect HTTP headers, cookies, and URLs for smarter routing decisions.`,
  },

  // ── Batch 3: security ─────────────────────────────────────────
  {
    path: 'kb/security/authn/jwt-best-practices.md',
    content: `---
confidence: 0.9
type: principle
entities:
  - JWT
  - authentication
refs:
  - https://datatracker.ietf.org/doc/html/rfc7519
---
# JWT Best Practices

Always validate the algorithm header to prevent algorithm confusion attacks. Use short-lived access tokens (5-15 min) with longer-lived refresh tokens. Never store sensitive data in JWT claims — they are base64-encoded, not encrypted.`,
  },
  {
    path: 'kb/security/authn/oauth2-pkce.md',
    content: `---
confidence: 0.85
type: principle
entities:
  - OAuth2
  - PKCE
  - authorization code flow
---
# OAuth2 PKCE Flow

PKCE (Proof Key for Code Exchange) mitigates authorization code interception attacks in public clients. The client generates a code_verifier and sends its SHA-256 hash as code_challenge during authorization.`,
  },
  {
    path: 'kb/security/encryption/aes-gcm.md',
    content: `---
confidence: 0.95
type: observation
entities:
  - AES-GCM
  - authenticated encryption
---
# AES-GCM Authenticated Encryption

AES-GCM provides both confidentiality and integrity in a single operation. It is the most widely recommended AEAD cipher. Nonce reuse is catastrophic — always use a unique 96-bit nonce per encryption.`,
  },
  {
    path: 'kb/security/supply-chain/sbom.md',
    content: `---
confidence: 0.6
type: observation
entities:
  - SBOM
  - supply chain security
---
# Software Bill of Materials

SBOMs (Software Bill of Materials) enumerate all dependencies in a software artifact. Formats like SPDX and CycloneDX are emerging standards. Adoption is growing but tooling maturity varies across ecosystems.`,
  },

  // ── Batch 4: observability ────────────────────────────────────
  {
    path: 'kb/observability/metrics/red-method.md',
    content: `---
confidence: 0.9
type: principle
entities:
  - RED method
  - metrics
  - SRE
---
# RED Method for Service Metrics

The RED method tracks three key signals for every service: Rate (requests/sec), Errors (failed requests/sec), and Duration (latency distribution). It is particularly suited for request-driven microservices.`,
  },
  {
    path: 'kb/observability/tracing/opentelemetry-basics.md',
    content: `---
confidence: 0.8
type: observation
entities:
  - OpenTelemetry
  - distributed tracing
  - spans
refs:
  - https://opentelemetry.io/docs/
---
# OpenTelemetry Basics

OpenTelemetry provides a vendor-neutral API for emitting traces, metrics, and logs. A trace consists of spans forming a DAG. Each span records operation name, timing, attributes, and parent context.`,
  },
  {
    path: 'kb/observability/logging/structured-logging.md',
    content: `---
confidence: 0.85
type: principle
entities:
  - structured logging
  - JSON logs
---
# Structured Logging

Structured logging (JSON format) enables machine-parseable log analysis. Key fields should include timestamp, level, message, trace_id, and service name. Avoid string interpolation in hot paths — use lazy evaluation.`,
  },
  {
    path: 'kb/observability/alerting/slo-burn-rate.md',
    content: `---
confidence: 0.7
type: observation
entities:
  - SLO
  - burn rate
  - alerting
---
# SLO Burn Rate Alerting

Burn rate alerting measures how fast an error budget is being consumed. A burn rate of 1x means the budget will be exhausted exactly at the SLO window end. Multi-window burn rates (fast + slow) reduce false positives.`,
  },

  // ── Batch 5: cross-domain and deep-path facts ─────────────────
  {
    path: 'kb/databases/postgresql/extensions/pgvector.md',
    content: `---
confidence: 0.8
type: observation
entities:
  - pgvector
  - PostgreSQL
  - vector search
---
# pgvector Extension

pgvector adds vector similarity search to PostgreSQL. It supports L2 distance, inner product, and cosine distance operators. IVFFlat and HNSW index types are available for approximate nearest neighbor search.`,
  },
  {
    path: 'kb/security/network/mtls.md',
    content: `---
confidence: 0.9
type: principle
entities:
  - mTLS
  - mutual TLS
  - zero trust
---
# Mutual TLS (mTLS)

mTLS requires both client and server to present certificates during the TLS handshake. It is a foundational building block for zero-trust network architectures and service mesh communication.`,
  },
  {
    path: 'kb/observability/dashboards/grafana-best-practices.md',
    content: `---
confidence: 0.3
type: observation
entities:
  - Grafana
  - dashboards
---
# Grafana Dashboard Best Practices

Dashboards should follow a hierarchy: high-level service overview -> subsystem drill-down -> individual component detail. Limit panels to 8-12 per dashboard. Use template variables for environment/service switching.`,
  },
  {
    path: 'kb/networking/service-mesh/envoy-proxy.md',
    content: `---
confidence: 0.6
type: observation
entities:
  - Envoy
  - service mesh
  - sidecar proxy
---
# Envoy Proxy in Service Meshes

Envoy is a high-performance L7 proxy used as the data plane in service meshes like Istio. It provides automatic retries, circuit breaking, rate limiting, and observability without application code changes.`,
  },
];

// ── Derived exports ─────────────────────────────────────────────

export const SEED_FACTS = facts;

export const SEED_PATHS = facts.map((f) => f.path);

// ── URL helpers ─────────────────────────────────────────────────

/** Encode a branch name for use in a URL path segment (replace / with :). */
export function encodeBranch(name: string): string {
  return name.replace(/\//g, ':');
}

/**
 * The repo every e2e test works in. knomit ships with NO repos — a fresh
 * server serves none, and no name is privileged — so the harness creates this
 * one itself via createRepo before any test runs.
 */
export const E2E_REPO = 'knomit';

/**
 * Create the e2e repo via POST /api/v1/repos. The endpoint streams NDJSON
 * progress events, so the body is drained to completion before returning:
 * the repo is not registered until the final "done" event has been written.
 */
export async function createRepo(baseURL: string, repo = E2E_REPO): Promise<void> {
  const res = await fetch(`${baseURL}/api/v1/repos`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ name: repo, mode: 'preset', ontology_preset: 'default' }),
  });
  if (!res.ok) {
    throw new Error(`Failed to create repo ${repo}: ${res.status} ${res.statusText}\n${await res.text()}`);
  }
  const body = await res.text();
  if (!body.includes('"type":"done"')) {
    throw new Error(`Repo ${repo} creation did not complete:\n${body}`);
  }
}

/**
 * Discover the agent branch for a repo by calling GET /api/v1/repos/{repo}/branches.
 * Returns the first branch starting with "agent/" (encoded for URLs), or the first
 * branch overall if none starts with "agent/".
 */
export async function discoverAgentBranch(baseURL: string, repo = E2E_REPO): Promise<string> {
  const res = await fetch(`${baseURL}/api/v1/repos/${repo}/branches`);
  if (!res.ok) {
    throw new Error(`Failed to list branches: ${res.status} ${res.statusText}`);
  }
  const body = await res.json();
  const branches: Array<{ name: string }> = body?._embedded?.branches ?? [];
  if (branches.length === 0) {
    throw new Error('No branches found for repo ' + repo);
  }
  const agentBranch = branches.find((b) => b.name.startsWith('agent/')) ?? branches[0];
  return encodeBranch(agentBranch.name);
}

// ── Seed function ───────────────────────────────────────────────

const BATCH_SIZE = 4;
const BATCH_DELAY_MS = 1_500;

export async function seedFixture(baseURL: string): Promise<void> {
  // Discover the agent branch before writing any facts.
  const encodedBranch = await discoverAgentBranch(baseURL);

  for (let i = 0; i < facts.length; i += BATCH_SIZE) {
    const batch = facts.slice(i, i + BATCH_SIZE);

    for (const fact of batch) {
      const res = await fetch(
        `${baseURL}/api/v1/repos/knomit/branches/${encodedBranch}/facts/${fact.path}`,
        {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ content: fact.content }),
        },
      );

      if (!res.ok) {
        const body = await res.text();
        throw new Error(
          `Failed to seed fact ${fact.path}: ${res.status} ${res.statusText}\n${body}`,
        );
      }
    }

    // Delay between batches to create distinct commits
    if (i + BATCH_SIZE < facts.length) {
      await new Promise((r) => setTimeout(r, BATCH_DELAY_MS));
    }
  }
}

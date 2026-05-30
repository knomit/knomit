-- Domain-tag token containment matching (de-hyphenize + stemmed tokens).
-- fact_domain_tokens holds one row per (fact, canonical domain, stemmed token).
-- Kept per-(fact, domain) — NOT flattened per-fact — so a multi-token query is
-- matched within a SINGLE domain (GROUP BY domain), never bleeding tokens across
-- two different domains on the same fact. Populated from the canonical domain on
-- every Upsert/rebuild; CASCADE-cleaned with the fact row. Derived index state:
-- rebuilt from git, no data backfill needed here (next rebuild populates it).
CREATE TABLE fact_domain_tokens (
    fact_id INTEGER NOT NULL REFERENCES facts(id) ON DELETE CASCADE,
    domain  TEXT NOT NULL,
    token   TEXT NOT NULL,
    PRIMARY KEY (fact_id, domain, token)
);
CREATE INDEX fact_domain_tokens_token ON fact_domain_tokens(token);

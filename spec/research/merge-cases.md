# Research: Multi-Machine Merge Cases

Four concrete scenarios that arise when two machines independently learn facts against the same knowledge base.

## Case 1: No Conflict — Disjoint Facts

Two branches add completely different facts. No file overlap.

```
laptop:  + know/security/cve-2024-1234.md
desktop: + know/people/alice/prefers-dark-mode.md
```

**Resolution:** Trivial. Git auto-merges. No LLM needed.

## Case 2: Same Fact, Independent Reinforcement

Both branches update the same fact file — different confidence bumps, different new sources.

```
laptop:  know/people/alice/likes-rock.md  (confidence: 0.85 → 0.9, sources: 3 → 4)
desktop: know/people/alice/likes-rock.md  (confidence: 0.85 → 0.88, sources: 3 → 5)
```

Git sees a conflict on the same file.

**Resolution (deterministic rules):**
- Confidence: take `max(0.9, 0.88)` = 0.9
- Sources: sum the deltas — `(4-3) + (5-3) + 3` = 6
- Body: merge if different paragraphs changed; flag for review if same paragraph diverged

## Case 3: Semantic Contradiction

No file-level conflict — different files — but the facts contradict each other.

```
laptop:  + know/projects/api/uses-rest.md    (confidence: 0.8)
desktop: + know/projects/api/uses-graphql.md  (confidence: 0.7)
```

Git merges cleanly (different files), but the knowledge base now contains a contradiction.

**Resolution (LLM required):**
- Option A: Keep higher confidence, forget the other
- Option B: Keep both with lowered confidence (maybe the API genuinely uses both)
- Option C: Flag for human review

**Detection:** Requires semantic analysis — compare new facts against existing facts in the same world/entity path. This is the hardest case and the one that most justifies LLM involvement in the merge process.

## Case 4: Obsolescence Across Branches

One branch learns a fact. Another branch (or a later commit on the same branch) deletes it.

```
laptop:  + know/security/cve-2024-1234.md  (committed 3 months ago)
desktop: forget know/security/cve-2024-1234.md  (committed yesterday, CVE was fixed)
```

**Resolution:** The delete wins — it's more recent and intentional. Recency + intentional deletion beats older addition.

**Edge case:** If the addition is more recent than the deletion, flag for review — someone may have re-learned a fact that was previously forgotten for good reason.

## Summary Table

| Case | Git conflict? | Semantic conflict? | Resolution |
|------|--------------|-------------------|------------|
| Disjoint facts | No | No | Auto-merge |
| Same fact reinforced | Yes | No | Deterministic rules |
| Contradictory facts | No | Yes | LLM review |
| Add vs delete | Maybe | Maybe | Recency + intent |

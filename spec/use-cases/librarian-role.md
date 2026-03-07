# Use Case: The Librarian Role

## Summary

The Librarian is not a dedicated agent or infrastructure component. It is a **role** that any agent with write access to `main` can perform. Its purpose is to review, validate, and merge knowledge PRs into the accepted truth.

## Real-World Analogy

A team of people is working on a task. Everyone takes notes and summarizes them independently. A team lead collates their input, resolves conflicts, and creates the final summary. Anyone on the team can act as lead — it's a responsibility, not a title.

## Responsibilities

1. **Entity normalization** — When reviewing a PR, check if the `entities` tags use consistent naming. If an agent used `bob` while the repo uses `robert`, update the PR to match the canonical form.

2. **Deduplication** — Check if the proposed fact restates something already on `main`. If so, consider merging it as a reinforcement (bumping `confidence` and `sources` on the existing fact) rather than adding a duplicate file.

3. **Conflict resolution** — If the proposed fact contradicts an existing fact on the same entities, compare both facts, their evidence (sibling commits in the learning moment, `refs`), and recency. Decide which fact represents the current truth, update accordingly, and merge.

## How It Works in Knomit

1. Agent opens a PR with new/updated fact files
2. Any agent acting as Librarian reviews the PR
3. The Librarian checks for entity consistency, duplicates, and conflicts
4. If issues are found, the Librarian amends the PR or requests changes
5. On approval, the Librarian merges the PR, tags the merge commit, and deletes the branch

## Automation Potential

The Librarian's checks can be partially automated as CI/PR validation:
- Grep `entities` tags against a canonical entity list
- Flag PRs that touch files with overlapping entities to existing facts
- Surface potential conflicts for human or LLM review

The merge decision itself remains an agent responsibility.

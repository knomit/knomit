---
name: knomit-guidance
description: Core knomit operating guidance and this workspace's source binding — consult before knowledge-base work.
---

This workspace is bound to knomit repo `{{.RepoName}}`, source slug `{{.Source}}`.
When writing `src://` refs, use `{{.Source}}` as the source (e.g. `src://{{.Source}}/path/to/file.go@<commit>`).

{{.Instructions}}

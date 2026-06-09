---
kind: pragmatic
type: policy
domain: [security]
confidence: 0.95
sources: 3
entities: [secrets-management]
refs: []
---
# Rotate production secrets every 90 days

All production secrets — API keys, database credentials, signing keys — must
be rotated at least every 90 days. Automation should drive rotation; manual
rotation is allowed only for keys outside the automation system.

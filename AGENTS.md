# Working on Guino

Use parallel agents whenever independent, bounded work can progress at the same
time. Keep dependent steps sequential; do not create agents merely to narrate or
repeat a sequential response. Give each implementation agent distinct file
ownership and share interface decisions before editing shared behavior.

For substantive work, repeat these review loops:

1. Plan, independent agent review, revise the plan.
2. Implement, independent code review, fix actionable findings.
3. Test, independent review of test results and coverage, fix and retest.

Continue until every participating reviewer has no unresolved actionable
findings for the completed scope. Evidence matters more than a vote: record
unavailable checks and external blockers explicitly, and never claim complete
release readiness while required checks remain unverified. Re-review fixes that
affect a reviewer's findings; avoid rerunning unchanged checks without a reason.

Preserve the inherited Den commit graph, authorship, tags, licenses and copyright
notices. Rename work belongs in new commits. Guino remains a useful local,
self-hosted runtime without a cloud account. Treat persisted resource names and
configuration compatibility separately from public branding.

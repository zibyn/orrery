## Spec changes

`GLOSSARY.md`, `docs/adr/` and `docs/spec/` are locked: the spec is the source of truth and code follows it. If the spec itself is wrong, open an issue first, then change the document, then the code. Do not modify a locked spec inside an implementation commit, and do not ratify a code-side shortcut into the spec after the fact. The M1 deviation this rule came from is archived in #65.

## Agent skills

### Issue tracker

Issues live as GitHub issues in `zibyn/stars-sandbox`, via the `gh` CLI. See `docs/agents/issue-tracker.md`.

### Triage labels

The five canonical triage roles, each label string equal to its name. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: `GLOSSARY.md` and `docs/adr/` at the repo root. See `docs/agents/domain.md`.

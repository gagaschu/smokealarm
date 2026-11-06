# smokealarm – instructions for Claude

- The specification is in SPEC.md, the approved plan in PLAN.md. Read both before any work. Rules in SPEC.md are binding.

## Git workflow
- One branch per phase, based on main: phase-<n>-<short-name>.
- Commit at logical steps with clear messages. Never push to main, never force-push.
- End of a phase: go build, go vet, go test and golangci-lint are green. Push the branch, open a pull request against main with a short summary (what's new, how to try it locally), then stop and wait.
- CI is in .github/workflows/ci.yml (tests, image build; push to ghcr.io on main and on v* tags). See docs/development.md.

## Models and debugging
- The session runs with opusplan: Opus in plan mode, Sonnet otherwise.
- Plan each phase in plan mode before implementing.
- If a bug is not fixed after two attempts or the cause is unclear: stop, summarize observations and hypotheses, and ask the user to switch to plan mode for root-cause analysis. No workarounds that hide symptoms.

## Environment
- No real cluster is available in cloud sessions. Verify with unit tests and fake objects; the user tests against the real cluster locally.

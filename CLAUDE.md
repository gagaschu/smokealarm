# smokealarm – instructions for Claude

- The specification is in SPEC.md, the approved plan in PLAN.md. Read both before any work. Rules in SPEC.md are binding.

## Git workflow
- One branch per phase, based on main: phase-<n>-<short-name>.
- Commit at logical steps with clear messages. Never push to main, never force-push.
- Never push to `dev`: the user moves it to try a branch in his test cluster.
- Every PR with a change users notice (behavior, configuration, chart values, docs they follow) adds a line under `## [Unreleased]` in CHANGELOG.md, in the Keep a Changelog groups (Added, Changed, Deprecated, Removed, Fixed, Security), written for users, with the variable or value name. Refactoring, tests and CI only need no entry; say so in the PR description.
- End of a phase: go build, go vet, go test and golangci-lint are green. Push the branch, open a pull request against main with a short summary (what's new, how to try it locally), then stop and wait.
- The image must build for amd64 and arm64 without QEMU (.forgejo/workflows/build.yml runs on a machine without it): cross-compile in the Dockerfile's build stage, no `RUN` in the final stage.
- CI is in .github/workflows/ci.yml (tests, image build; image pushed to ghcr.io on main and on v* tags, the Helm chart as OCI on v* tags). See docs/development.md.

## Models and debugging
- The session runs with opusplan: Opus in plan mode, Sonnet otherwise.
- Plan each phase in plan mode before implementing.
- If a bug is not fixed after two attempts or the cause is unclear: stop, summarize observations and hypotheses, and ask the user to switch to plan mode for root-cause analysis. No workarounds that hide symptoms.

## Environment
- No real cluster is available in cloud sessions. Verify with unit tests and fake objects; the user tests against the real cluster locally.

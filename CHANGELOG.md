# Changelog

All notable changes to this project are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project aims to follow
[Semantic Versioning](https://semver.org/).

## Unreleased

Release notes start with the first public release; until then, see the Git history.

### Added

- `SMOKEALARM_TIME_ZONE` (Helm `config.timeZone`, default `UTC`): the time zone for times in the web interface, the downtime form, mails, chat messages and the times in check messages. The time fields of the webhook (`since`, `resolved_at`) and the stored data stay in UTC; the webhook `subject` and `message` carry the same texts as the other targets. A message template that writes "UTC" by hand next to a time should use `MST` in the layout instead ([docs/how-it-works.md](docs/how-it-works.md#time-zone)).
- Login with OIDC next to the admin password (`SMOKEALARM_OIDC_*`, Helm `oidc.*`, [docs/oidc.md](docs/oidc.md)). Access by an allowlist of verified emails and/or groups; the local admin stays as the way in when the provider is down. New dependencies: `github.com/coreos/go-oidc/v3` and `golang.org/x/oauth2`.

# Changelog

All notable changes to this project are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project aims to follow
[Semantic Versioning](https://semver.org/).

## Unreleased

Release notes start with the first public release; until then, see the Git history.

### Added

- Login with OIDC next to the admin password (`SMOKEALARM_OIDC_*`, Helm `oidc.*`, [docs/oidc.md](docs/oidc.md)). Access by an allowlist of verified emails and/or groups; the local admin stays as the way in when the provider is down. New dependencies: `github.com/coreos/go-oidc/v3` and `golang.org/x/oauth2`.

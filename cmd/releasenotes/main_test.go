package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const changelog = `# Changelog

Intro.

## [Unreleased]

### Added

- Something new.

## [0.2.0] - 2026-11-10

### Added

- Feature A.

### Fixed

- Bug B.

## [0.1.0] - 2026-11-06

The first release.

[Unreleased]: https://example.com/compare/v0.2.0...HEAD
[0.2.0]: https://example.com/compare/v0.1.0...v0.2.0
[0.1.0]: https://example.com/releases/tag/v0.1.0
`

func TestNotesFor(t *testing.T) {
	tests := []struct {
		version, want string
	}{
		{"v0.2.0", "### Added\n\n- Feature A.\n\n### Fixed\n\n- Bug B.\n\n**Full Changelog**: https://example.com/compare/v0.1.0...v0.2.0\n"},
		{"0.2.0", "### Added\n\n- Feature A.\n\n### Fixed\n\n- Bug B.\n\n**Full Changelog**: https://example.com/compare/v0.1.0...v0.2.0\n"},
		// The last section ends at the link definitions.
		{"v0.1.0", "The first release.\n\n**Full Changelog**: https://example.com/releases/tag/v0.1.0\n"},
		// A pre-release of a version that has its section takes that one.
		// Without the compare link: it names the tag v0.2.0.
		{"v0.2.0-rc.1", "### Added\n\n- Feature A.\n\n### Fixed\n\n- Bug B.\n"},
		// Otherwise Unreleased, without its compare link to HEAD.
		{"v0.3.0-rc.1", "### Added\n\n- Something new.\n"},
	}
	for _, tt := range tests {
		got, err := notesFor(changelog, tt.version)
		if err != nil {
			t.Errorf("notesFor(%q): %v", tt.version, err)
			continue
		}
		if got != tt.want {
			t.Errorf("notesFor(%q) =\n%s\nwant\n%s", tt.version, got, tt.want)
		}
	}
}

func TestNotesForSkipsEmptySection(t *testing.T) {
	cl := strings.Replace(changelog, "## [0.2.0] - 2026-11-10", "## [0.3.0] - TBD\n\n## [0.2.0] - 2026-11-10", 1)
	got, err := notesFor(cl, "v0.3.0-rc.1")
	if err != nil {
		t.Fatal(err)
	}
	if want := "### Added\n\n- Something new.\n"; got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

// A link definition inside a section does not end it.
func TestNotesForKeepsLinkDefinitionInSection(t *testing.T) {
	cl := strings.Replace(changelog, "- Feature A.\n", "- Feature A, see [docs].\n\n[docs]: https://example.com/docs\n", 1)
	got, err := notesFor(cl, "v0.2.0")
	if err != nil {
		t.Fatal(err)
	}
	want := "### Added\n\n- Feature A, see [docs].\n\n[docs]: https://example.com/docs\n\n### Fixed\n\n- Bug B.\n\n**Full Changelog**: https://example.com/compare/v0.1.0...v0.2.0\n"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestNotesForErrors(t *testing.T) {
	empty := strings.Replace(changelog, "### Added\n\n- Something new.\n\n", "", 1)
	tests := []struct {
		name, changelog, version, want string
	}{
		{"release does not fall back to Unreleased", changelog, "v0.3.0", "no section"},
		{"empty Unreleased for a pre-release", empty, "v0.3.0-rc.1", "section [Unreleased] in the changelog is empty"},
		{"empty release section", strings.Replace(changelog, "## [0.2.0] - 2026-11-10", "## [0.3.0] - TBD\n\n## [0.2.0] - 2026-11-10", 1), "v0.3.0", "section [0.3.0] in the changelog is empty"},
		{"not a version", changelog, "main", "not a version"},
		{"partial version", changelog, "v0.2", "not a version"},
	}
	for _, tt := range tests {
		_, err := notesFor(tt.changelog, tt.version)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want it to contain %q", tt.name, err, tt.want)
		}
	}
}

func TestRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "CHANGELOG.md")
	if err := os.WriteFile(path, []byte(changelog), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run([]string{"-changelog", path, "v0.1.0"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "The first release.") {
		t.Errorf("output = %q", out.String())
	}
	if err := run([]string{"-changelog", path}, &out); err == nil {
		t.Error("no error without a version")
	}
}

// The repository's own changelog must have a section for the chart's version,
// so a release tag does not fail in CI.
func TestOwnChangelogHasChartVersion(t *testing.T) {
	chart, err := os.ReadFile("../../deploy/helm/smokealarm/Chart.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var version string
	for line := range strings.SplitSeq(string(chart), "\n") {
		if v, ok := strings.CutPrefix(line, "version: "); ok {
			version = strings.TrimSpace(v)
		}
	}
	data, err := os.ReadFile("../../CHANGELOG.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := notesFor(string(data), version); err != nil {
		t.Errorf("chart version %s: %v", version, err)
	}
}

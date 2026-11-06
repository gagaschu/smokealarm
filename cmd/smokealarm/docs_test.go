package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gagaschu/smokealarm/internal/checks"
)

// The docs, the Helm chart and the Dockerfile are reference material for
// users. These tests make sure they do not silently fall behind the code or
// contradict each other: every environment variable and every check must be
// documented, the chart may only set variables the program reads, and the
// Dockerfile builds with the Go version of go.mod.

func readFile(t *testing.T, parts ...string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(parts...))
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Join(parts...), err)
	}
	return string(data)
}

// variableName matches the name of a SMOKEALARM_* environment variable.
var variableName = regexp.MustCompile(`SMOKEALARM_[A-Z_]+`)

// configVariables returns every SMOKEALARM_* variable that the config code
// mentions, which is the list of variables the program reads.
func configVariables(t *testing.T) map[string]bool {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", "internal", "config", "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no config sources found: %v", err)
	}
	found := map[string]bool{}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		for _, v := range variableName.FindAllString(readFile(t, file), -1) {
			found[v] = true
		}
	}
	if len(found) < 20 {
		t.Fatalf("found only %d variables in the config code; is the pattern still right?", len(found))
	}
	return found
}

func TestDocsDescribeEveryEnvironmentVariable(t *testing.T) {
	doc := readFile(t, "..", "..", "docs", "configuration.md")
	for variable := range configVariables(t) {
		if !strings.Contains(doc, "`"+variable+"`") {
			t.Errorf("docs/configuration.md does not describe %s", variable)
		}
	}
}

func TestDocsListEveryCheck(t *testing.T) {
	doc := readFile(t, "..", "..", "docs", "checks.md")
	for _, c := range checks.All() {
		if !strings.Contains(doc, "`"+c.Name+"`") {
			t.Errorf("docs/checks.md does not list the check %s", c.Name)
		}
	}
}

// markdownLink matches the target of a Markdown link or an <img src>.
var markdownLink = regexp.MustCompile(`\]\(([^)\s]+)\)|src="([^"]+)"`)

// headingAnchor turns a Markdown heading into the anchor GitHub gives it.
func headingAnchor(heading string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(heading) {
		switch {
		case r == ' ':
			b.WriteRune('-')
		case r == '-' || r == '_' || ('a' <= r && r <= 'z') || ('0' <= r && r <= '9'):
			b.WriteRune(r)
		}
	}
	return b.String()
}

// anchors returns the anchors of all headings in a Markdown text.
func anchors(text string) map[string]bool {
	found := map[string]bool{}
	inCode := false
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "```") {
			inCode = !inCode
		}
		if !inCode && strings.HasPrefix(line, "#") {
			found[headingAnchor(strings.TrimSpace(strings.TrimLeft(line, "#")))] = true
		}
	}
	return found
}

// The README links to the docs, and the docs link to each other and to files
// in the repository. Moving or renaming a file or a heading must not leave a
// dead link behind.
func TestDocsLinksPointToExistingFiles(t *testing.T) {
	root := filepath.Join("..", "..")
	docs, err := filepath.Glob(filepath.Join(root, "docs", "*.md"))
	if err != nil || len(docs) == 0 {
		t.Fatalf("no docs found: %v", err)
	}
	files := append([]string{filepath.Join(root, "README.md")}, docs...)
	for _, file := range files {
		text := readFile(t, file)
		for _, m := range markdownLink.FindAllStringSubmatch(text, -1) {
			target := m[1] + m[2]
			if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			path, anchor, _ := strings.Cut(target, "#")
			linked := file
			if path != "" {
				linked = filepath.Join(filepath.Dir(file), path)
				if _, err := os.Stat(linked); err != nil {
					t.Errorf("%s links to %s, which does not exist", file, target)
					continue
				}
			}
			if anchor != "" && strings.HasSuffix(linked, ".md") && !anchors(readFile(t, linked))[anchor] {
				t.Errorf("%s links to %s, but there is no such heading", file, target)
			}
		}
	}
}

// config.Load ignores variables it does not know, so a typo in the chart
// (SMOKEALARM_CRONJOB_GRAC) would silently leave the default in place. Every
// variable name in the chart has to be one that the config code reads.
func TestChartUsesOnlyKnownEnvironmentVariables(t *testing.T) {
	chart := filepath.Join("..", "..", "deploy", "helm", "smokealarm")
	templates, err := filepath.Glob(filepath.Join(chart, "templates", "*"))
	if err != nil || len(templates) == 0 {
		t.Fatalf("no chart templates found: %v", err)
	}
	files := append(templates, filepath.Join(chart, "values.yaml"))

	known := configVariables(t)
	seen := 0
	for _, file := range files {
		for _, v := range variableName.FindAllString(readFile(t, file), -1) {
			seen++
			if !known[v] {
				t.Errorf("%s uses %s, which the program does not read (typo?)", file, v)
			}
		}
	}
	if seen < 15 {
		t.Fatalf("found only %d variable names in the chart; is the pattern still right?", seen)
	}
}

// The Dockerfile has a default Go version so that a plain "docker build ." works.
// "make image" passes the version from go.mod; the default must not drift away
// from it, or a direct build would use another toolchain.
func TestDockerfileGoVersionMatchesGoMod(t *testing.T) {
	dockerfile := regexp.MustCompile(`(?m)^ARG GO_VERSION=(\S+)$`).FindStringSubmatch(readFile(t, "..", "..", "Dockerfile"))
	goMod := regexp.MustCompile(`(?m)^go (\S+)$`).FindStringSubmatch(readFile(t, "..", "..", "go.mod"))
	if dockerfile == nil || goMod == nil {
		t.Fatalf("could not find the Go version: Dockerfile %v, go.mod %v", dockerfile, goMod)
	}
	if dockerfile[1] != goMod[1] {
		t.Errorf("Dockerfile has ARG GO_VERSION=%s but go.mod says go %s; update the Dockerfile", dockerfile[1], goMod[1])
	}
}

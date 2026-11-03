package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gagaschu/smokealarm/internal/checks"
)

// The README, the Helm chart and the Dockerfile are reference material for
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

func TestReadmeDocumentsEveryEnvironmentVariable(t *testing.T) {
	readme := readFile(t, "..", "..", "README.md")
	for variable := range configVariables(t) {
		if !strings.Contains(readme, "`"+variable+"`") {
			t.Errorf("README.md does not document %s", variable)
		}
	}
}

func TestReadmeListsEveryCheck(t *testing.T) {
	readme := readFile(t, "..", "..", "README.md")
	for _, c := range checks.All() {
		if !strings.Contains(readme, "`"+c.Name+"`") {
			t.Errorf("README.md does not list the check %s", c.Name)
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

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gagaschu/smokealarm/internal/checks"
)

// The README is the reference for users. These tests make sure it does not
// silently fall behind the code: every environment variable and every check
// must be mentioned.

func readFile(t *testing.T, parts ...string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(parts...))
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Join(parts...), err)
	}
	return string(data)
}

func TestReadmeDocumentsEveryEnvironmentVariable(t *testing.T) {
	readme := readFile(t, "..", "..", "README.md")

	files, err := filepath.Glob(filepath.Join("..", "..", "internal", "config", "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no config sources found: %v", err)
	}
	name := regexp.MustCompile(`SMOKEALARM_[A-Z_]+`)
	found := map[string]bool{}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		for _, v := range name.FindAllString(readFile(t, file), -1) {
			found[v] = true
		}
	}
	if len(found) < 20 {
		t.Fatalf("found only %d variables in the config code; is the pattern still right?", len(found))
	}
	for variable := range found {
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

func TestChartValuesMentionEveryEnvironmentVariable(t *testing.T) {
	// The chart maps settings to variables; values.yaml names each one in a
	// comment. A variable the chart does not know about is fine (extraEnv),
	// but the ones it sets must be spelled correctly.
	values := readFile(t, "..", "..", "deploy", "helm", "smokealarm", "values.yaml")
	readme := readFile(t, "..", "..", "README.md")
	name := regexp.MustCompile(`SMOKEALARM_[A-Z_]+`)
	for _, v := range name.FindAllString(values, -1) {
		if !strings.Contains(readme, "`"+v+"`") {
			t.Errorf("values.yaml mentions %s, which the README does not know", v)
		}
	}
}

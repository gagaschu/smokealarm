// Command releasenotes prints the release notes for a version from CHANGELOG.md.
// CI uses it for the GitHub release of a version tag:
//
//	go run ./cmd/releasenotes v0.2.0
//
// A release (v1.2.3) needs its own section "## [1.2.3] - <date>". A pre-release
// (v1.2.3-rc.1) takes its own section, else the one of 1.2.3, else
// "## [Unreleased]"; an empty section is skipped. It fails if there is no
// non-empty section, so a tag without a changelog fails before anything is
// built.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "releasenotes:", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("releasenotes", flag.ContinueOnError)
	changelog := fs.String("changelog", "CHANGELOG.md", "path to the changelog")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: releasenotes [-changelog CHANGELOG.md] <version>")
	}
	data, err := os.ReadFile(*changelog)
	if err != nil {
		return err
	}
	notes, err := notesFor(string(data), fs.Arg(0))
	if err != nil {
		return err
	}
	_, err = io.WriteString(out, notes)
	return err
}

var versionRe = regexp.MustCompile(`^v?([0-9]+\.[0-9]+\.[0-9]+)(-[0-9A-Za-z.-]+)?$`)

// notesFor returns the section for version (with or without "v"), followed by
// the compare link of that section if the changelog has one.
func notesFor(changelog, version string) (string, error) {
	m := versionRe.FindStringSubmatch(version)
	if m == nil {
		return "", fmt.Errorf("%q is not a version like v1.2.3 or v1.2.3-rc.1", version)
	}
	full := m[1] + m[2]
	names := []string{full}
	if m[2] != "" {
		names = append(names, m[1], "Unreleased")
	}
	empty := ""
	for _, name := range names {
		body, ok := section(changelog, name)
		if !ok {
			continue
		}
		if body == "" {
			if empty == "" {
				empty = name
			}
			continue
		}
		// Only the tag's own section: the link of 1.2.3 for v1.2.3-rc.1 would
		// point at a tag that does not exist yet.
		if link, ok := linkFor(changelog, name); ok && name == full {
			body += "\n\n**Full Changelog**: " + link
		}
		return body + "\n", nil
	}
	if empty != "" {
		return "", fmt.Errorf("section [%s] in the changelog is empty", empty)
	}
	return "", fmt.Errorf("the changelog has no section [%s]", strings.Join(names, "], ["))
}

// section returns the trimmed text under "## [name]" up to the next "## "
// heading. The last section ends before the link definitions at the end of
// the changelog; link definitions inside a section are kept.
func section(changelog, name string) (string, bool) {
	var body []string
	found, last := false, true
	for line := range strings.SplitSeq(changelog, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "## ") {
			if found {
				last = false
				break
			}
			found = line == "## ["+name+"]" || strings.HasPrefix(line, "## ["+name+"] ")
			continue
		}
		if found {
			body = append(body, line)
		}
	}
	if last {
		for len(body) > 0 {
			l := body[len(body)-1]
			if strings.TrimSpace(l) != "" && !linkRe.MatchString(l) {
				break
			}
			body = body[:len(body)-1]
		}
	}
	return strings.TrimSpace(strings.Join(body, "\n")), found
}

var linkRe = regexp.MustCompile(`^\[[^\]]+\]: \S+$`)

// linkFor returns the URL of the link definition "[name]: <url>".
func linkFor(changelog, name string) (string, bool) {
	for line := range strings.SplitSeq(changelog, "\n") {
		if url, ok := strings.CutPrefix(strings.TrimRight(line, "\r"), "["+name+"]: "); ok {
			return strings.TrimSpace(url), true
		}
	}
	return "", false
}

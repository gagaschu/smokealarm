package notify

import (
	"bytes"
	"embed"
	"fmt"
	"strings"
	"text/template"
)

// builtinFiles are the built-in message templates. A user template replaces
// one of them (see Templates); without any, these are the messages.
//
//go:embed templates/*.tmpl
var builtinFiles embed.FS

// The parts of a message that have a template, by their file names.
const (
	subjectFile = "subject.tmpl"
	mailFile    = "mail.tmpl"
	chatFile    = "chat.tmpl"
)

// builtinTemplate parses one of the built-in templates. Parsing takes
// microseconds, so it is done for each message instead of keeping state.
func builtinTemplate(name string) (*template.Template, error) {
	tmpl, err := template.ParseFS(builtinFiles, "templates/"+name)
	if err != nil {
		return nil, fmt.Errorf("parse built-in template %s: %w", name, err)
	}
	return tmpl, nil
}

// render executes a template.
func render(tmpl *template.Template, data templateData) (string, error) {
	var out bytes.Buffer
	if err := tmpl.Execute(&out, data); err != nil {
		return "", fmt.Errorf("render %s: %w", tmpl.Name(), err)
	}
	return out.String(), nil
}

// maxSubjectLength is the longest subject in bytes. A template may list
// every problem in it, but the subject goes into a mail header line (at most
// 998 bytes, after the encoding of non-ASCII characters, which can triple
// them) and in front of the Telegram text, which shares its 4096 characters
// with it. The built-in subjects are far shorter.
const maxSubjectLength = 200

// renderSubject renders a subject, puts it on one line and cuts it at
// maxSubjectLength. It goes into the mail header and the ntfy header, where a
// line break would start a header of its own.
func renderSubject(tmpl *template.Template, data templateData) (string, error) {
	subject, err := render(tmpl, data)
	if err != nil {
		return "", err
	}
	return cutText(oneLine(subject), maxSubjectLength), nil
}

// renderChat renders a chat text without spaces and blank lines at its start
// and end, which the services would show or drop depending on the client.
func renderChat(tmpl *template.Template, data templateData) (string, error) {
	text, err := render(tmpl, data)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(text), nil
}

// fitChat renders a chat text of at most limit bytes. Bytes, not characters,
// because that is exact for ntfy and on the safe side for the services that
// count characters.
//
// If the whole text is too long, it is rendered with fewer problems, the last
// ones left out first, and .Omitted tells the template how many are missing,
// so it can say so and where to see them. The number that fits is found by
// halving the range (binary search): trying one problem less each time took a
// minute and a half for a Teams message with a thousand problems. The result
// always fits; with a template whose text does not grow evenly with the
// problems it may show a few less than would fit. A text that says nothing
// does not count as fitting (a template that only lists problems renders
// nothing without them). If nothing fits, the text with the fewest problems
// that says something is cut.
func fitChat(tmpl *template.Template, data templateData, limit int) (string, error) {
	text, err := renderChat(tmpl, data)
	if err != nil || len(text) <= limit {
		return text, err
	}
	fitting := ""
	shortest, shortestKeep := text, data.problemCount() // to cut if nothing fits
	low, high := 0, data.problemCount()-1               // the numbers of problems still to try
	for low <= high {
		keep := (low + high) / 2
		text, err := renderChat(tmpl, data.keepFirst(keep))
		if err != nil {
			return "", err
		}
		switch {
		case text == "":
			low = keep + 1 // it needs more problems to say something
		case len(text) <= limit:
			fitting = text
			low = keep + 1
		default:
			high = keep - 1
		}
		if text != "" && keep < shortestKeep {
			shortest, shortestKeep = text, keep
		}
	}
	if fitting != "" {
		return fitting, nil
	}
	return cutText(shortest, limit), nil
}

package notify

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/template"
	"time"

	"github.com/gagaschu/smokealarm/internal/checks"
)

// maxTemplateSize is the largest template file that is read. A message
// template is a few hundred bytes; anything this large is a mistake.
const maxTemplateSize = 64 << 10

// The parts of a message that a user template can replace.
const (
	partSubject = "subject"
	partMail    = "mail"
	partChat    = "chat"
)

// emptyMessage is the error of a template that renders nothing. A constant
// and not an error variable, because the code has no package-level variables.
const emptyMessage = "renders an empty text"

// Templates are the message templates of the user, read from
// SMOKEALARM_TEMPLATE_DIR. A target uses its own file first, then the general
// one, then the built-in template:
//
//	subject.tmpl          the subject of every target
//	<kind>.subject.tmpl   the subject of one kind (mail, ntfy, telegram, slack, teams)
//	mail.tmpl             the mail body
//	chat.tmpl             the text of every chat target
//	<kind>.chat.tmpl      the text of one chat target (ntfy, telegram, slack, teams)
//
// A nil *Templates means the built-in templates only.
type Templates struct {
	files map[string]*template.Template // by file name
}

// kindsOf returns the kinds of targets that use a part.
func kindsOf(part string) []string {
	switch part {
	case partSubject:
		return []string{"mail", "ntfy", "telegram", "slack", "teams"}
	case partChat:
		return []string{"ntfy", "telegram", "slack", "teams"}
	default:
		return []string{"mail"}
	}
}

// templateFiles maps every valid file name to the kinds of targets it is for.
func templateFiles() map[string][]string {
	files := map[string][]string{}
	for _, part := range []string{partSubject, partMail, partChat} {
		files[part+".tmpl"] = kindsOf(part)
		if part == partMail {
			continue // the mail body is for mail only
		}
		for _, kind := range kindsOf(part) {
			files[kind+"."+part+".tmpl"] = []string{kind}
		}
	}
	return files
}

// LoadTemplates reads the templates in dir. An empty dir means no user
// templates (nil, nil). Every file is parsed and rendered with made-up
// notifications for every target it is for, so a mistake (an unknown field, a
// syntax error, a file name with a typo) stops the start with the file and the
// line, instead of showing up in the first alarm. Hidden entries (a ConfigMap
// volume keeps its files in "..data"), directories and files without the
// .tmpl ending are skipped.
func LoadTemplates(dir string) (*Templates, error) {
	if dir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read template directory: %w", err)
	}
	valid := templateFiles()
	t := &Templates{files: map[string]*template.Template{}}
	var errs []error
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".tmpl") {
			continue
		}
		tmpl, err := loadTemplateFile(filepath.Join(dir, name), valid)
		if err != nil {
			errs = append(errs, fmt.Errorf("template %s: %w", name, err))
			continue
		}
		if tmpl != nil {
			t.files[name] = tmpl
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	if len(t.files) == 0 {
		return nil, nil
	}
	return t, nil
}

// loadTemplateFile reads, parses and checks one file. It returns nil without
// an error for a directory. The files of a ConfigMap volume are links, so the
// type of the entry says nothing; os.Stat follows the link.
func loadTemplateFile(path string, valid map[string][]string) (*template.Template, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, nil
	}
	name := filepath.Base(path)
	kinds, found := valid[name]
	if !found {
		return nil, fmt.Errorf("unknown file name; valid are %s", strings.Join(slices.Sorted(maps.Keys(valid)), ", "))
	}
	if info.Size() > maxTemplateSize {
		return nil, fmt.Errorf("larger than %d KiB", maxTemplateSize>>10)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// The name of the template is the file name, so that errors from
	// text/template ("template: chat.tmpl:3: ...") name the file.
	tmpl, err := template.New(name).Parse(string(content))
	if err != nil {
		return nil, err
	}
	if err := checkTemplate(tmpl, partOf(name), kinds); err != nil {
		return nil, err
	}
	return tmpl, nil
}

// partOf is the part a valid file name is for: "slack.chat.tmpl" is chat.
func partOf(name string) string {
	parts := strings.Split(strings.TrimSuffix(name, ".tmpl"), ".")
	return parts[len(parts)-1]
}

// checkTemplate renders a template with every sample, for every kind of
// target it is for, so that a branch for one target or for a shortened text is
// checked too. Only the full sample must give some text: a template may well
// say nothing about a notification that has nothing it shows.
func checkTemplate(tmpl *template.Template, part string, kinds []string) error {
	for _, kind := range kinds {
		for i, data := range sampleData(kind) {
			text, err := renderPart(tmpl, part, data)
			if err != nil {
				return err
			}
			if i == 0 && text == "" {
				return errors.New(emptyMessage)
			}
		}
	}
	return nil
}

// renderPart renders one part without shortening: a subject on one line, a
// chat text trimmed, a mail as it is.
func renderPart(tmpl *template.Template, part string, data templateData) (string, error) {
	switch part {
	case partSubject:
		return renderSubject(tmpl, data)
	case partChat:
		return renderChat(tmpl, data)
	default:
		text, err := render(tmpl, data)
		if strings.TrimSpace(text) == "" {
			text = ""
		}
		return text, err
	}
}

// sampleData are the made-up notifications for checking a template. The
// first has something in every list and field, including problems left out
// of a shortened chat text; the others have little, as real ones often do.
func sampleData(kind string) []templateData {
	since := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	pod := func(name string, severity checks.Severity) Alert {
		return Alert{
			Finding: checks.Finding{
				Check: "pod-crashloop", Severity: severity, Reason: "CrashLoopBackOff",
				Object:  checks.ObjectRef{Kind: checks.KindPod, Namespace: "media", Name: name},
				Message: "container keeps crashing",
			},
			Since: since,
		}
	}
	node := Alert{
		Finding: checks.Finding{
			Check: "node-not-ready", Severity: checks.Critical, Reason: "NotReady",
			Object: checks.ObjectRef{Kind: checks.KindNode, Name: "pi-1"}, Message: "node is not ready",
		},
		Since:    since,
		Affected: []AffectedProblem{{Object: checks.ObjectRef{Kind: checks.KindPod, Namespace: "media", Name: "app"}, Reason: "CrashLoopBackOff"}},
	}
	gone := pod("old", checks.Warning)
	gone.ResolvedAt = since.Add(time.Hour)

	full := newTemplateData(Notification{
		New:       []Alert{node, pod("jellyfin", checks.Critical), pod("sonarr", checks.Warning)},
		Reminders: []Alert{pod("radarr", checks.Critical), node},
		Resolved:  []Alert{gone},
		BaseURL:   "https://smokealarm.example.com",
	}, kind)
	full.Omitted = 3
	return []templateData{
		full,
		newTemplateData(Notification{New: []Alert{pod("jellyfin", checks.Critical)}}, kind),
		newTemplateData(Notification{Resolved: []Alert{gone}}, kind),
	}
}

// lookup finds the user template for a part and a kind of target: its own
// file first, then the general one. It returns nil if there is none.
func (t *Templates) lookup(kind, part string) (*template.Template, string) {
	if t == nil {
		return nil, ""
	}
	names := []string{part + ".tmpl"}
	if part != partMail {
		names = []string{kind + "." + part + ".tmpl", part + ".tmpl"}
	}
	for _, name := range names {
		if tmpl, found := t.files[name]; found {
			return tmpl, name
		}
	}
	return nil, ""
}

// Describe lists the user templates a kind of target uses, for the status
// page: "subject.tmpl, slack.chat.tmpl", or "built-in" if none. It is empty for
// targets without templates (webhook, log).
func (t *Templates) Describe(kind string) string {
	var parts []string
	for _, part := range []string{partSubject, partMail, partChat} {
		if slices.Contains(kindsOf(part), kind) {
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	var files []string
	for _, part := range parts {
		if _, name := t.lookup(kind, part); name != "" {
			files = append(files, name)
		}
	}
	if len(files) == 0 {
		return "built-in"
	}
	return strings.Join(files, ", ")
}

// UnusedFiles lists, sorted, the files that no target of the given kinds
// reads, for a warning at startup: a template for a target that is switched
// off is allowed, but may also be a mistake.
func (t *Templates) UnusedFiles(kinds []string) []string {
	if t == nil {
		return nil
	}
	valid := templateFiles()
	var unused []string
	for name := range t.files {
		used := false
		for _, kind := range valid[name] {
			if slices.Contains(kinds, kind) {
				used = true
			}
		}
		if !used {
			unused = append(unused, name)
		}
	}
	slices.Sort(unused)
	return unused
}

// FallbackError says that a user template failed while a message was
// rendered, and that the message was sent with the built-in template instead.
// Send returns it only after the message went out, so the engine counts the
// message as delivered and shows the error on the status page. An alarm must
// not stay silent because of a template.
type FallbackError struct {
	File string // e.g. "slack.chat.tmpl"
	Err  error
}

func (e *FallbackError) Error() string {
	return fmt.Sprintf("template %s failed, sent with the built-in one: %v", e.File, e.Err)
}

func (e *FallbackError) Unwrap() error { return e.Err }

// firstFallback returns the first of the fallbacks that is set, as an error,
// or nil. A nil *FallbackError must not become a non-nil error.
func firstFallback(fallbacks ...*FallbackError) error {
	for _, fallback := range fallbacks {
		if fallback != nil {
			return fallback
		}
	}
	return nil
}

// subject renders the subject for a kind of target.
func (t *Templates) subject(kind string, n Notification) (string, *FallbackError) {
	tmpl, file := t.lookup(kind, partSubject)
	if tmpl == nil {
		return Subject(n), nil
	}
	subject, err := renderSubject(tmpl, newTemplateData(n, kind))
	if err == nil && subject == "" {
		err = errors.New(emptyMessage)
	}
	if err != nil {
		return Subject(n), &FallbackError{File: file, Err: err}
	}
	return subject, nil
}

// mailBody renders the mail body. The error is that of the built-in template,
// which only a bug can cause.
func (t *Templates) mailBody(n Notification) (string, *FallbackError, error) {
	tmpl, file := t.lookup("mail", partMail)
	if tmpl != nil {
		body, err := render(tmpl, newTemplateData(n, "mail"))
		if err == nil && strings.TrimSpace(body) == "" {
			err = errors.New(emptyMessage)
		}
		if err == nil {
			return body, nil, nil
		}
		builtin, builtinErr := Body(n)
		return builtin, &FallbackError{File: file, Err: err}, builtinErr
	}
	body, err := Body(n)
	return body, nil, err
}

// chatBody renders the chat text for a kind of target, at most limit bytes
// long (see fitChat).
func (t *Templates) chatBody(kind string, n Notification, limit int) (string, *FallbackError) {
	tmpl, file := t.lookup(kind, partChat)
	if tmpl == nil {
		return ChatBody(n, limit), nil
	}
	text, err := fitChat(tmpl, newTemplateData(n, kind), limit)
	if err == nil && text == "" {
		err = errors.New(emptyMessage)
	}
	if err != nil {
		return ChatBody(n, limit), &FallbackError{File: file, Err: err}
	}
	return text, nil
}

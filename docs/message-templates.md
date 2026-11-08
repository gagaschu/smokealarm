# Message templates

You can change the wording and layout of the messages with your own templates, for example to write them in German, name the cluster in every subject, or make the phone message shorter. Templates cover the subject and the text of mail, ntfy, Telegram, Slack and Teams, and the HTML part of the mail. The [webhook](notifications.md#webhook) keeps its JSON format.

They are Go [text/template](https://pkg.go.dev/text/template) files (the HTML part: [html/template](https://pkg.go.dev/html/template)) in the directory `SMOKEALARM_TEMPLATE_DIR`. In the chart you set them under `templates` (file name → content); see [values.yaml](../deploy/helm/smokealarm/values.yaml) and the German example [message-templates-de.yaml](../deploy/helm/examples/message-templates-de.yaml).

## Files

| File | Used for |
|---|---|
| `subject.tmpl` | The subject of every target: the mail subject, the ntfy title, the first line on Telegram, the header on Slack and Teams |
| `mail.tmpl` | The text part of the mail |
| `mail.html.tmpl` | The HTML part of the mail, an html/template (see below) |
| `chat.tmpl` | The text of ntfy, Telegram, Slack and Teams |
| `<target>.subject.tmpl` | The subject of one target: `mail`, `ntfy`, `telegram`, `slack` or `teams` |
| `<target>.chat.tmpl` | The text of one chat target: `ntfy`, `telegram`, `slack` or `teams` |

A target uses its own file first, then the general one, then the built-in template, so you only write what you want to change. `telegram` covers all chats. The built-in templates in [internal/notify/templates](../internal/notify/templates) are a good place to start.

A short chat text in German:

```
{{range .NodesDown}}Knoten {{.Name}} ausgefallen, {{len .Affected}} betroffen
{{end}}{{range .New}}{{if eq .Severity "critical"}}KRITISCH{{else}}Warnung{{end}} {{.Object}}: {{.Reason}} (seit {{.Since.Format "02.01. 15:04 MST"}})
{{end}}{{range .Resolved}}Behoben: {{.Object}}: {{.Reason}}
{{end}}{{if .Omitted}}… und {{.Omitted}} weitere
{{end}}{{.BaseURL}}
```

### The HTML mail

A mail has a text part and an HTML part; mail programs show the HTML one. `mail.html.tmpl` is an [html/template](https://pkg.go.dev/html/template): it gets the same fields and functions, and escapes every value, so a message from the cluster can never add markup or a link, and a `.BaseURL` that is not a web address becomes `#ZgotmplZ`. Mail programs know little CSS, so use tables and inline `style` attributes, and nothing loaded from elsewhere (no images, no web fonts).

If you have your own `mail.tmpl` but no `mail.html.tmpl`, the mail is sent as text only, because mail programs would otherwise show the built-in English HTML and hide your text. To keep your text and get HTML too, add a `mail.html.tmpl`; the German example has one. The status page then says "text only" next to the templates of the mail. If your `mail.html.tmpl` fails with a real notification, the mail gets the built-in HTML instead, or goes out as text only if you also have a `mail.tmpl`.

## Fields

Every template gets the same fields:

| Field | Content |
|---|---|
| `.Target` | `mail`, `ntfy`, `telegram`, `slack` or `teams`, so one `chat.tmpl` can differ a little per target |
| `.Summary` | The built-in summary, e.g. `3 new problems, 1 resolved` or `NODE DOWN: pi-2 (+ 1 resolved)` |
| `.NodesDown` | Nodes that are newly down, each with `.Affected`: the problems it holds back |
| `.New` | The other new problems, critical first |
| `.Reminders` | Critical problems that are still open (see `SMOKEALARM_CRITICAL_REPEAT_INTERVAL`) |
| `.Resolved` | Problems that went away |
| `.BaseURL` | `SMOKEALARM_BASE_URL`, empty if not set |
| `.Omitted` | How many problems were left out of a chat text that was too long; always 0 for subjects and mails |

Each problem in these lists has:

| Field | Content |
|---|---|
| `.Severity` | `critical` or `warning` |
| `.Check`, `.Reason` | e.g. `pod-crashloop`, `CrashLoopBackOff` |
| `.Kind`, `.Namespace`, `.Name` | e.g. `Pod`, `media`, `jellyfin`; the namespace is empty for nodes |
| `.Object` | The object as one label: `Pod media/jellyfin` |
| `.Message` | The message, always on one line |
| `.Duration` | How long a resolved problem lasted: `3m`, `2h 10m`, `1d 4h` or `less than a minute`; empty unless resolved |
| `.Since`, `.ResolvedAt` | When it was first seen and when it was resolved (zero unless resolved), in the time zone `SMOKEALARM_TIME_ZONE` (UTC by default). `MST` in a layout writes its abbreviation, for example `CET`. Format them with a Go layout: `{{.Since.Format "02.01.2006 15:04"}}` |
| `.Affected` | For a node that is down: the problems it holds back, each with `.Object` and `.Reason` |

Only the functions that come with text/template are available: `if`, `range`, `with`, `eq`, `ne`, `len`, `index`, `printf`, `and`, `or`, `not` and a few more. Times are in `SMOKEALARM_TIME_ZONE`, UTC by default. A template that writes the text "UTC" next to a time shows the wrong zone once you set another one: use `MST` in the layout instead.

The subject is put on one line after rendering, so line breaks in `subject.tmpl` only help readability and can never add a mail header. It is cut at 200 bytes, so it fits into a mail header line and leaves room for the text on Telegram. Spaces and blank lines at the start and end of a chat text are removed.

## Long messages

If a chat text is longer than the service allows (see the limits in [Chat messages](notifications.md#chat-messages)), smokealarm renders it again with fewer problems, dropping the last ones first, and sets `.Omitted` to the number it left out. Your template can then say `… und 3 weitere`. If not even one problem fits, the shortest version is cut off with `…`. Mails are not shortened. Subjects are cut at 200 bytes, the Slack header at 150 characters.

## Mistakes in a template

At startup smokealarm renders every file with made-up notifications, for every target the file is for. These stop the start, with the file name and the line:

- a syntax error,
- an unknown field (`{{.Sinse}}`),
- a file name that isn't in the table above (`slak.chat.tmpl`),
- a template that renders nothing,
- `html.tmpl` or `<target>.html.tmpl`: the HTML part is `mail.html.tmpl` only.

A file for a target that isn't configured, and a file whose name doesn't end in `.tmpl` (`chat.tpl`), are only logged as a warning.

A template can still fail with a real notification, for example `{{index .New 5}}` when there are fewer problems, or it can render nothing. That message then goes out with the built-in template instead, so the alarm still arrives. The log and the status page say "built-in template used", with the error. The status page also lists the templates each target uses.

Templates are read at startup only, so restart smokealarm after a change. The chart does that for you with a checksum annotation on the pod.

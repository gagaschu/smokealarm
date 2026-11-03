# Auftrag: smokealarm – leichtgewichtiges Kubernetes-Health-Monitoring in Go

## Ziel
Baue ein kleines Open-Source-Tool namens smokealarm, das im Kubernetes-Cluster läuft und genau eine Frage beantwortet: „Was ist gerade kaputt?“
Leitbild: „A smoke alarm for your Kubernetes cluster – silent until something's on fire.“
Keine Metrik-Datenbank, keine Graphen, keine Logs, keine Traces, keine Root-Cause-Analyse. Stattdessen: wenige feste, zuverlässige Checks ohne Konfigurationsaufwand, eine einfache Web-Oberfläche mit aktuellen Problemen und Historie, und Benachrichtigung per Mail bei Zustandswechseln.
Primäre Testumgebung: k3s auf Raspberry Pi (arm64). Es muss aber auf jedem Standard-Kubernetes laufen und darf nichts k3s-Spezifisches voraussetzen.

## Wichtigste Anforderung: lesbarer, einfacher Code
Der Maintainer ist kein Go-Entwickler. Der Code muss für jemanden mit Ops-Hintergrund ohne Go-Erfahrung nachvollziehbar sein. Lesbarkeit hat Vorrang vor Eleganz, Performance und Abstraktion.
- Idiomatisches, schlichtes Go (Effective Go, Go Code Review Comments). gofmt, go vet und golangci-lint mit Standard-Lintern müssen sauber sein.
- Explizit statt clever: keine Generics, keine Reflection, keine init()-Funktionen, keine globalen Variablen außer Konstanten. Abhängigkeiten werden in main() erzeugt und als Struct-Felder weitergegeben.
- Interfaces nur dort, wo es real mehrere Implementierungen gibt (z. B. Notifier: Mail jetzt, Telegram/Webhook später). Keine Interfaces auf Vorrat.
- Nebenläufigkeit minimal: Informers, eine Hauptschleife mit Ticker, der HTTP-Server. Keine Channel-Pipelines, keine Worker-Pools.
- Kurze Funktionen (Richtwert < 50 Zeilen), sprechende Namen. Fehler immer mit Kontext wrappen (fmt.Errorf("load config: %w", err)), nie verschlucken.
- Kommentare erklären das Warum, nicht das Was. Jedes Paket hat einen Package-Doc-Kommentar. Jeder Check hat einen Doc-Kommentar: was wird geprüft, warum ist das ein Problem, welche Schwere.
- Code, Kommentare, Log-Meldungen und Doku auf Englisch.
- Logging mit log/slog (JSON im Cluster, Text lokal).
- Zeit über eine injizierbare now-Funktion, damit Logik und Tests deterministisch sind.

## Abhängigkeiten
Erlaubt, sonst nur Standard-Library:
- k8s.io/client-go, k8s.io/api, k8s.io/apimachinery
- modernc.org/sqlite (pure Go, damit CGO_ENABLED=0 und einfaches Cross-Compiling)
- golang.org/x/crypto (bcrypt)
- github.com/robfig/cron/v3 (nur zum Parsen von CronJob-Schedules)
Ausdrücklich nicht: controller-runtime, cobra, viper, Web-Frameworks (gin, echo, chi), ORMs, Prometheus-Libraries, Frontend-Build-Tooling.
HTTP-Routing mit net/http (Go-1.22+-Patterns), HTML mit html/template, Mail mit net/smtp und text/template, SQL mit database/sql.
Weitere Abhängigkeiten nur nach Rückfrage.

## Projektstruktur
cmd/smokealarm/main.go   Verdrahtung: Config laden, Clients, Store, Engine, Webserver starten; Subcommand hash-password
internal/config          Config aus Env-Variablen, Defaults, Validierung
internal/kube            Clients, Informers (core + dynamic für CRDs), Erkennung optionaler CRDs
internal/checks          Eine Datei pro Bereich (pods.go, workloads.go, nodes.go, jobs.go, storage.go, certs.go, argocd.go) plus Tests
internal/engine          Hauptschleife: Checks ausführen, mit gespeichertem Zustand abgleichen, Benachrichtigungen auslösen
internal/store           SQLite-Zugriff, Schema-Migrationen als eingebettete .sql-Dateien, Retention
internal/notify          Notifier-Interface, Mail-Implementierung
internal/auth            Lokaler Login, Sessions, Middleware
internal/web             Handler, Templates, CSS (alles per go:embed)
deploy/helm/smokealarm   Helm-Chart
Keine weiteren Pakete ohne guten Grund.

## Kernkonzept
Ein Check ist eine einfache Funktion: Er bekommt einen Snapshot der Cluster-Objekte aus dem Informer-Cache und gibt eine Liste von Findings zurück. Checks rufen nie selbst die API auf und haben keinen eigenen Zustand – dadurch sind sie trivial mit Fake-Objekten testbar. (Ausnahme: PVC-Füllstand, siehe unten – die Kubelet-Stats werden vor den Checks in den Snapshot geladen.)

Finding (Richtschnur):
- Key: stabil aus Check + Kind/Namespace/Name + Grund; identifiziert „dasselbe Problem“ über Läufe hinweg
- Check, Severity (critical | warning), Object (Kind, Namespace, Name)
- Reason: kurz, z. B. "CrashLoopBackOff"
- Message: menschenlesbar mit relevantem Kontext (z. B. letzter Exit-Code, passendes Warning-Event)

Ablauf alle SMOKEALARM_CHECK_INTERVAL (Default 30s):
1. Snapshot bauen, alle aktivierten Checks ausführen.
2. Mit dem Zustand in SQLite abgleichen: neues Problem → pending; besteht länger als SMOKEALARM_ALERT_AFTER (Default 2m) → firing; verschwunden → resolved (nur wenn es vorher firing war, sonst still verwerfen). Kurze Flaps werden so nicht gemeldet.
3. Alle noch nicht gemeldeten Wechsel (firing/resolved) als eine Sammel-Benachrichtigung verschicken. Erst nach erfolgreichem Versand als gemeldet markieren; schlägt der Versand fehl, wird es im nächsten Lauf erneut versucht. Das überlebt auch Neustarts.
4. Resolved-Findings älter als SMOKEALARM_RETENTION (Default 30d) löschen.

## Checks (v0.1)
Pods
- CrashLoopBackOff → critical
- ImagePullBackOff / ErrImagePull → critical
- OOMKilled (letzte Termination innerhalb der letzten 15 min) → warning
- Viele Restarts: restartCount > Schwellwert (Default 5) und letzter Restart innerhalb der letzten Stunde → warning
- Pending länger als Schwellwert (Default 5m), mit Grund aus Conditions/Events (z. B. Unschedulable) → warning
Workloads
- Deployment/StatefulSet/DaemonSet mit weniger verfügbaren als gewünschten Replicas → warning; 0 verfügbar bei gewünscht > 0 → critical
Nodes
- NotReady → critical
- MemoryPressure / DiskPressure / PIDPressure → warning
Jobs
- Job fehlgeschlagen (Condition Failed) → warning. Bei CronJobs zählt nur der jeweils neueste Job, damit alte Fehlschläge nicht dauerhaft alarmieren.
- CronJob hat einen geplanten Lauf verpasst (laut Cron-Ausdruck fällig, lastScheduleTime älter als erwartet + Toleranz) → warning. Suspendierte CronJobs ignorieren.
Storage
- PVC Pending länger als Schwellwert → warning
- PVC-Füllstand über Kubelet-Stats (/api/v1/nodes/<node>/proxy/stats/summary): > 85 % warning, > 95 % critical. Abschaltbar, weil dafür die RBAC-Berechtigung nodes/proxy nötig ist.
Zertifikate (nur wenn cert-manager-CRDs vorhanden)
- Certificate nicht Ready → warning
- status.notAfter in < 14 Tagen → warning, < 3 Tagen → critical
- Keine Secrets lesen.
Argo CD (nur wenn Argo-CD-CRDs vorhanden)
- Application Health Degraded oder Missing → critical
- Sync OutOfSync → warning (ALERT_AFTER dämpft kurze Syncs)
Optionale CRDs werden beim Start per Discovery-API erkannt. Fehlen sie, wird der Check übersprungen, geloggt und auf der Status-Seite angezeigt.
Einzelne Checks lassen sich per Env-Variable deaktivieren.

Ignorieren:
- Annotation smokealarm.<DOMAIN>/ignore: "true" am Objekt oder an seinem Namespace unterdrückt Findings. Für Pods genügt die Annotation im Pod-Template. Das Annotation-Präfix ist eine einzige Konstante im Code.
- Zusätzlich Env-Variable mit Liste ignorierter Namespaces.

## Benachrichtigung
- v0.1: nur Mail per SMTP (STARTTLS und implizites TLS auf Port 465, optional mit Auth, mehrere Empfänger).
- Pro Lauf höchstens eine Mail mit allen neuen Zustandswechseln.
- Betreff fasst zusammen: bei einem Wechsel konkret, z. B. "[smokealarm] CRITICAL: CrashLoopBackOff media/jellyfin"; bei mehreren z. B. "[smokealarm] 3 new problems, 1 resolved".
- Body als Plaintext: neue Probleme nach Schwere sortiert, danach behobene; jeweils Objekt, Grund, Message, seit wann; Link zur Web-UI (SMOKEALARM_BASE_URL).
- Notifier-Interface so schneiden, dass Telegram und generischer Webhook später als eigene Datei dazukommen, ohne die Engine zu ändern. Selbst noch nicht implementieren.

## Web-UI
- Server-gerendertes HTML, kein JavaScript-Framework, kein Build-Schritt. Auto-Refresh per Meta-Tag (30s). Schlichtes eigenes CSS, hell/dunkel per prefers-color-scheme, auf dem Handy gut nutzbar.
- Seiten:
  - Login
  - Übersicht: aktuelle Probleme nach Schwere mit Objekt, Grund, Message, seit wann. Leerer Zustand „All good“ mit Zeitpunkt des letzten Laufs.
  - Historie: behobene Findings, filterbar nach Namespace
  - Status: letzter Lauf, aktive/übersprungene Checks mit Grund, ob Mail konfiguriert ist, letzter Versandfehler
- /healthz (Liveness) und /readyz (Informer-Caches synchronisiert, DB erreichbar), beide ohne Auth.

## Authentifizierung
- v0.1: ein lokaler Admin-User; Username und bcrypt-Hash aus Env (im Cluster aus einem Secret).
- Login-Formular, serverseitige Sessions in SQLite mit zufälliger Session-ID. Cookie HttpOnly, Secure (für lokale Entwicklung abschaltbar), SameSite=Lax, Ablauf konfigurierbar (Default 7d). Logout. Einfaches Rate-Limiting für fehlgeschlagene Logins.
- Subcommand `smokealarm hash-password` liest ein Passwort von stdin und gibt den bcrypt-Hash aus (einfacher os.Args-Switch, kein CLI-Framework).
- Aufbau so, dass OIDC später als zweite Login-Methode dazukommt: Die Middleware prüft nur „gibt es eine gültige Session?“; wie die Session entsteht (Passwort, später OIDC), ist davon getrennt. OIDC selbst nicht implementieren.

## Konfiguration
- Nur Env-Variablen mit Präfix SMOKEALARM_. Alles hat sinnvolle Defaults außer Pflichtwerten (Admin-User, Passwort-Hash, SMTP-Daten wenn Mail aktiv).
- Beim Start validieren; bei Fehlern mit klarer Meldung abbrechen.
- Läuft im Cluster (In-Cluster-Config) und lokal gegen KUBECONFIG – für die Entwicklung auf macOS (Apple Silicon) gegen einen lokalen k3s.
- Vollständige Tabelle aller Variablen in der README.

## Sicherheit
- RBAC: ausschließlich get/list/watch, minimale Ressourcenliste. nodes/proxy nur bei aktivem PVC-Füllstand-Check; CRD-Rechte nur für cert-manager Certificates und Argo CD Applications, jeweils per Helm-Value schaltbar.
- Container: nonroot, readOnlyRootFilesystem, alle Capabilities gedroppt, seccompProfile RuntimeDefault, allowPrivilegeEscalation false. Nur das PVC ist beschreibbar.
- Keine Passwörter, Hashes oder Session-IDs im Log.
- Informer-Speicher klein halten: managedFields und nicht benötigte Felder per SetTransform entfernen.

## Packaging & Open-Source-Grundausstattung
- Dockerfile: Multi-Stage, CGO_ENABLED=0, Ziel gcr.io/distroless/static:nonroot, Multi-Arch (linux/amd64, linux/arm64) per docker buildx.
- Makefile mit build, test, lint, run (lokal), image.
- Helm-Chart: Deployment (1 Replica, Strategy Recreate wegen SQLite auf RWO-PVC), PVC, Service, optionales Ingress, ServiceAccount, RBAC mit Schaltern für optionale Checks, Secret oder existingSecret für Passwort-Hash und SMTP-Zugangsdaten, Liveness/Readiness-Probes, Resources (Richtwert requests 64Mi/10m, limit 128Mi), securityContext wie oben.
- LICENSE (Apache-2.0)
- README: Leitbild-Satz, Was/Warum, klare Abgrenzung (kein Prometheus-Ersatz, keine Root-Cause-Analyse, keine Traces – bewusst wenige, zuverlässige Checks), Installation per Helm, Konfigurationstabelle, Liste aller Checks mit Schwere, lokale Entwicklung.
- ARCHITECTURE.md: eine Seite für Leser ohne Go-Erfahrung – Datenfluss, Pakete und ihre Aufgabe, Schritt-für-Schritt „Wie füge ich einen neuen Check hinzu“.
- CHANGELOG.md, .gitignore.
- Git-Workflow siehe CLAUDE.md. 

## Tests
- Table-driven Tests für jeden Check mit selbst gebauten Objekten (keine echte API), inklusive Grenzfällen: knapp unter/über Schwellwert, per Annotation ignoriert, optionale CRDs fehlen.
- Tests für die Zustandslogik der Engine gegen eine temporäre SQLite-Datei: pending → firing → resolved, Flap wird unterdrückt, fehlgeschlagener Versand wird wiederholt, Sammel-Mail bei mehreren Wechseln.
- Golden-Test für Betreff und Body der Mail.

## Vorgehen
Erstelle zuerst PLAN.md mit Paketaufbau, zentralen Typen, Datenbankschema, Liste der Env-Variablen und Phasenplan. Warte dann auf meine Freigabe.
Danach in Phasen. Jede Phase endet mit lauffähigem Stand, grünem go build / go vet / go test / golangci-lint und einer kurzen Zusammenfassung: was ist neu, wie probiere ich es lokal aus. Nach jeder Phase anhalten und auf Freigabe warten.
1. Grundgerüst: main, Config, Logging, Informers, Pod-Checks, Findings ins Log, /healthz. Lokal gegen KUBECONFIG lauffähig.
2. Restliche Kern-Checks (Workloads, Nodes, Jobs, CronJobs, PVC Pending) mit Tests.
3. SQLite-Store, Zustandslogik, Retention.
4. Mail-Benachrichtigung mit Sammel-Mails.
5. Web-UI und Login.
6. Optionale Checks: PVC-Füllstand, cert-manager, Argo CD.
7. Packaging: Dockerfile, Makefile, Helm-Chart, README, ARCHITECTURE.md, LICENSE, CHANGELOG.
Wenn eine Anforderung unklar ist oder sich widerspricht: nachfragen statt raten. Bewusste Abweichungen vom Auftrag kurz begründen.

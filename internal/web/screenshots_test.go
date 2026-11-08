package web

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/gagaschu/smokealarm/internal/checks"
	"github.com/gagaschu/smokealarm/internal/engine"
	"github.com/gagaschu/smokealarm/internal/store"
)

// The screenshots in the README and the docs are taken by TestScreenshots. Normal test runs
// skip it. After a change to the look, take them again with
//
//	make screenshots
//
// and look at the new images before committing them. The test needs Google
// Chrome or Chromium (set CHROME to its path if it is not found) and, to keep
// the files small, pngquant.

// screenshotDir is where the README and the docs expect the images, seen from this package.
const screenshotDir = "../../docs/screenshots"

// screenshot is one image for the README or the docs.
type screenshot struct {
	file   string // file name in screenshotDir
	path   string // page on the test server, see screenshotPages
	dark   bool   // dark mode instead of light mode
	width  int    // browser window in CSS pixels; the image has twice as many
	height int
	trim   bool // cut off the empty background below the content
}

func screenshots() []screenshot {
	return []screenshot{
		// The window is taller than the page; trim removes the rest.
		{file: "overview.png", path: "/overview", width: 1100, height: 3000, trim: true},
		{file: "history.png", path: "/history", width: 1100, height: 3000, trim: true},
		{file: "status.png", path: "/status", dark: true, width: 1100, height: 3000, trim: true},
		// The size of a phone screen: only the top of the page.
		{file: "phone-all-good.png", path: "/all-good", dark: true, width: 390, height: 844},
		{file: "phone-overview.png", path: "/overview", dark: true, width: 390, height: 844},
	}
}

func TestScreenshots(t *testing.T) {
	if os.Getenv("UPDATE_SCREENSHOTS") != "1" {
		t.Skip("set UPDATE_SCREENSHOTS=1 to take the screenshots for the docs (make screenshots)")
	}
	chrome, ok := findChrome()
	if !ok {
		t.Fatal("Google Chrome or Chromium not found; set CHROME to its path")
	}
	server := httptest.NewServer(screenshotPages(t))
	defer server.Close()

	for _, shot := range screenshots() {
		file, err := filepath.Abs(filepath.Join(screenshotDir, shot.file))
		if err != nil {
			t.Fatal(err)
		}
		takeScreenshot(t, chrome, server.URL+shot.path, file, shot)
		if shot.trim {
			trimBackground(t, file)
		}
		compressPNG(t, file)
		t.Logf("wrote %s", file)
	}
}

// screenshotPages renders the pages from made-up data through the real
// handlers. The returned handler serves them without a login, so the browser
// needs no session, and gets the stylesheet and icons from the real handler.
func screenshotPages(t *testing.T) http.Handler {
	e := newTestEnv(t)
	cookie := e.login()
	e.status = screenshotStatus()
	get := func(path string) string {
		return e.do(http.MethodGet, path, nil, cookie).Body.String()
	}

	pages := map[string]string{}
	pages["/all-good"] = get("/") // before there are any problems
	// ntfy is switched off from here on, so the overview and the status page
	// show it; the all-good page above stays as it was.
	if rec := e.do(http.MethodPost, "/targets/ntfy/mute", nil, cookie); rec.Code != http.StatusSeeOther {
		t.Fatalf("switch ntfy off: status %d", rec.Code)
	}
	insertScreenshotProblems(t, e, cookie)
	insertScreenshotHistory(t, e)
	pages["/overview"] = get("/")
	pages["/history"] = get("/history")
	pages["/status"] = get("/status")

	mux := http.NewServeMux()
	mux.Handle("GET /static/", e.handler)
	for path, html := range pages {
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = io.WriteString(w, html) // a failed write shows up in the image
		})
	}
	return securityHeaders(mux)
}

// screenshotStatus is what the engine reports: mail and ntfy are set up, and Argo CD is
// not installed, so its checks are skipped.
func screenshotStatus() engine.Status {
	status := engine.Status{
		LastRun: testNow.Add(-12 * time.Second),
		Targets: []engine.TargetStatus{
			{Name: "mail", Description: "mail (smtp.example.com)", LastSentAt: testNow.Add(-2 * time.Hour)},
			{Name: "ntfy", Description: "ntfy (ntfy.sh)", LastSentAt: testNow.Add(-2 * time.Hour)},
		},
	}
	for _, check := range checks.All() {
		state := engine.CheckStatus{Name: check.Name, Active: true}
		if check.Requires == checks.RequiresArgoCD {
			state = engine.CheckStatus{
				Name:   check.Name,
				Reason: "Argo CD CRDs not installed (applications.argoproj.io); checked at startup",
			}
		}
		status.Checks = append(status.Checks, state)
	}
	return status
}

// screenshotProblem is a firing problem with a message as the check writes it.
func screenshotProblem(kind, namespace, name, reason, message string, severity checks.Severity, since time.Duration) store.Record {
	return store.Record{
		Finding: checks.Finding{
			Key:      kind + "/" + namespace + "/" + name + "/" + reason,
			Check:    "screenshot",
			Severity: severity,
			Object:   checks.ObjectRef{Kind: kind, Namespace: namespace, Name: name},
			Reason:   reason,
			Message:  message,
		},
		State:     store.StateFiring,
		FirstSeen: testNow.Add(-since),
		LastSeen:  testNow,
	}
}

// screenshotResolved is a problem that lasted for a while and ended ago.
func screenshotResolved(kind, namespace, name, reason, message string, severity checks.Severity, lasted, ago time.Duration) store.Record {
	record := screenshotProblem(kind, namespace, name, reason, message, severity, ago+lasted)
	resolvedAt := testNow.Add(-ago)
	record.State = store.StateResolved
	record.ResolvedAt = &resolvedAt
	return record
}

// insertScreenshotProblems fills the overview: three critical problems, one
// of them acknowledged, and two warnings, one of them in a downtime.
func insertScreenshotProblems(t *testing.T, e *testEnv, cookie *http.Cookie) {
	t.Helper()
	node := screenshotProblem("Node", "", "pi-node-3", "NotReady",
		"node is not ready for 26m0s (status Unknown)", checks.Critical, 26*time.Minute)
	volume := screenshotProblem("PersistentVolumeClaim", "nextcloud", "data-nextcloud-0", "VolumeAlmostFull",
		"88.2% full (44.1 GiB of 50.0 GiB used)", checks.Warning, 3*24*time.Hour+4*time.Hour)
	e.insert(
		screenshotProblem("Pod", "media", "jellyfin-7d9f8b6c4-x2kqp", "CrashLoopBackOff",
			"container jellyfin keeps crashing (14 restarts), last exit code 1 (Error)", checks.Critical, 2*time.Hour+14*time.Minute),
		node,
		screenshotProblem("Pod", "home", "homeassistant-0", "ImagePullBackOff",
			`container homeassistant cannot pull image ghcr.io/home-assistant/home-assistant:2026.13.0: Back-off pulling image "ghcr.io/home-assistant/home-assistant:2026.13.0"`,
			checks.Critical, 9*time.Minute),
		volume,
		screenshotProblem("Pod", "monitoring", "loki-0", "TooManyRestarts",
			"container loki restarted 7 times, last exit code 2 (Error)", checks.Warning, 50*time.Minute),
	)
	e.do(http.MethodPost, acknowledgePath(e.openRecord(node.Key).ID), url.Values{}, cookie)
	setScreenshotDowntime(t, e, volume, 20*time.Hour+30*time.Minute)
}

// insertScreenshotHistory fills the history with resolved problems, one of
// them in a downtime because it keeps coming back.
func insertScreenshotHistory(t *testing.T, e *testEnv) {
	t.Helper()
	cronJob := screenshotResolved("CronJob", "backup", "restic-nightly", "MissedSchedule",
		"expected run at 2026-11-02T02:00:00Z, last run: 2026-11-01T02:00:04Z", checks.Warning, 24*time.Hour, 6*time.Hour)
	e.insert(
		screenshotResolved("Deployment", "media", "sonarr", "NoReplicasAvailable",
			"0/1 replicas available", checks.Critical, 30*time.Minute, 4*time.Hour+30*time.Minute),
		cronJob,
		screenshotResolved("Certificate", "ingress", "wildcard-example-org", "ExpiresSoon",
			"expires in 12d 4h (2026-11-14T16:00:00Z)", checks.Warning, 24*time.Hour, 26*time.Hour),
		screenshotResolved("Node", "", "pi-node-2", "DiskPressure",
			"node reports DiskPressure since 2026-11-01T21:40:12Z", checks.Warning, 80*time.Minute, 37*time.Hour),
		screenshotResolved("Pod", "home", "zigbee2mqtt-6b7c9d-wq8vz", "OOMKilled",
			"container zigbee2mqtt was OOMKilled at 2026-10-31T04:02:11Z", checks.Warning, time.Hour, 79*time.Hour),
	)
	setScreenshotDowntime(t, e, cronJob, 3*24*time.Hour)
}

func setScreenshotDowntime(t *testing.T, e *testEnv, record store.Record, length time.Duration) {
	t.Helper()
	err := e.store.SetDowntime(t.Context(), store.Downtime{
		Key: record.Key, Check: record.Check, Object: record.Object, Reason: record.Reason,
		CreatedAt: testNow, ExpiresAt: testNow.Add(length),
	})
	if err != nil {
		t.Fatalf("set downtime: %v", err)
	}
}

// findChrome returns the path of Google Chrome or Chromium. CHROME overrides
// the search.
func findChrome() (string, bool) {
	if path := os.Getenv("CHROME"); path != "" {
		return path, true
	}
	for _, name := range []string{
		"google-chrome", "chromium", "chromium-browser",
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
	} {
		if path, err := exec.LookPath(name); err == nil {
			return path, true
		}
	}
	return "", false
}

// takeScreenshot opens the page in a headless Chrome and saves what the window
// shows as a PNG file. Headless Chrome uses a temporary profile, not yours.
func takeScreenshot(t *testing.T, chrome, pageURL, file string, shot screenshot) {
	t.Helper()
	args := []string{
		"--headless",
		"--disable-gpu",
		"--hide-scrollbars",
		"--force-device-scale-factor=2",  // sharp on high-resolution screens
		"--force-prefers-reduced-motion", // no animation caught halfway
		fmt.Sprintf("--window-size=%d,%d", shot.width, shot.height),
		"--screenshot=" + file,
	}
	if shot.dark {
		args = append(args, "--force-dark-mode")
	}
	if os.Geteuid() == 0 {
		// Chrome does not start as root with its sandbox, e.g. in a container.
		args = append(args, "--no-sandbox")
	}

	// One picture takes about a second. The limit turns a hanging browser into
	// a clear error; WaitDelay stops waiting for its helper processes.
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, chrome, append(args, pageURL)...)
	cmd.WaitDelay = 5 * time.Second
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("chrome: %v\n%s", err, output)
	}
}

// trimBackground cuts off the empty page background below the content, but
// keeps a small margin. Chrome can only take a picture of its window, so the
// window is taller than the page.
func trimBackground(t *testing.T, file string) {
	t.Helper()
	img := readPNG(t, file)
	bounds := img.Bounds()
	background := img.At(bounds.Min.X, bounds.Max.Y-1)

	lastContent := bounds.Min.Y
	for y := bounds.Max.Y - 1; y > bounds.Min.Y; y-- {
		if !rowHasOnly(img, y, background) {
			lastContent = y
			break
		}
	}
	const margin = 64 // 32 CSS pixels at twice the density
	bottom := min(lastContent+margin, bounds.Max.Y)

	trimmed := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bottom-bounds.Min.Y))
	draw.Draw(trimmed, trimmed.Bounds(), img, bounds.Min, draw.Src)
	writePNG(t, file, trimmed)
}

// rowHasOnly reports whether every pixel in row y has the given color.
func rowHasOnly(img image.Image, y int, want color.Color) bool {
	wr, wg, wb, wa := want.RGBA()
	for x := img.Bounds().Min.X; x < img.Bounds().Max.X; x++ {
		r, g, b, a := img.At(x, y).RGBA()
		if r != wr || g != wg || b != wb || a != wa {
			return false
		}
	}
	return true
}

func readPNG(t *testing.T, file string) image.Image {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode %s: %v", file, err)
	}
	return img
}

func writePNG(t *testing.T, file string, img image.Image) {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode %s: %v", file, err)
	}
	if err := os.WriteFile(file, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// compressPNG reduces the image to 256 colors with pngquant, which keeps even
// small colored dots. Without pngquant the file stays as it is, about three
// times larger.
func compressPNG(t *testing.T, file string) {
	t.Helper()
	pngquant, err := exec.LookPath("pngquant")
	if err != nil {
		t.Logf("pngquant not found, %s is not compressed", file)
		return
	}
	output, err := exec.Command(pngquant, "--force", "--strip", "--speed=1", "--output", file, "--", file).CombinedOutput()
	if err != nil {
		t.Fatalf("pngquant: %v\n%s", err, output)
	}
}

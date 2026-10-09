package web

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"spendbot/internal/store"
	"spendbot/internal/update"
)

func TestNotesHTML(t *testing.T) {
	got := string(notesHTML("## What's new\n\n- **Income** page\n- `spendbot update`\n\n| Computer | Archive |\n| --- | --- |\n| Mac | `x.zip` |\n\n1. Unzip\n2. Run\n\n<script>alert(1)</script> [site](https://example.com) [bad](javascript:alert(1))"))
	for _, want := range []string{"<h4>What&#39;s new</h4>", "<li><b>Income</b> page</li>", "<code>spendbot update</code>",
		"<td>Mac</td>", "<ol><li>Unzip</li><li>Run</li></ol>", "&lt;script&gt;", `<a href="https://example.com"`} {
		if !strings.Contains(got, want) {
			t.Errorf("notes lack %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "<script>") || strings.Contains(got, `href="javascript`) || strings.Contains(got, "<td>---</td>") {
		t.Errorf("unsafe or separator row:\n%s", got)
	}
}

func fakeRelease(t *testing.T, tag string) *httptest.Server {
	t.Helper()
	name := update.ArchiveName(tag, runtime.GOOS, runtime.GOARCH)
	bin := "spendbot"
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	var data bytes.Buffer
	if strings.HasSuffix(name, ".zip") {
		zw := zip.NewWriter(&data)
		w, _ := zw.Create("spendbot-" + tag + "/" + bin)
		w.Write([]byte("new program"))
		zw.Close()
	} else {
		t.Skip("the fake release builds a zip; this test runs on macOS and Windows")
	}
	sum := sha256.Sum256(data.Bytes())
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/" + update.Repo + "/releases/latest":
			json.NewEncoder(w).Encode(update.Release{Tag: tag, Body: "## What's new\n- shiny", URL: "https://example.com/r",
				Assets: []update.Asset{{Name: name, URL: srv.URL + "/a"}, {Name: "SHA256SUMS.txt", URL: srv.URL + "/s"}}})
		case "/a":
			w.Write(data.Bytes())
		case "/s":
			io.WriteString(w, hex.EncodeToString(sum[:])+"  "+name+"\n")
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestUpdateFromSettings(t *testing.T) {
	gh := fakeRelease(t, "v9.9.9")
	exe := filepath.Join(t.TempDir(), "spendbot")
	os.WriteFile(exe, []byte("old program"), 0o755)
	st, err := store.Open(context.Background(), t.TempDir()+"/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	up := update.New("v0.5.0", false, slogDiscard())
	up.Point(gh.URL, func() (string, error) { return exe, nil })
	restarted := make(chan bool, 1)
	srv := New(st, nil, 3, time.Minute, almaty, slogDiscard()).WithSettings(Settings{Updater: up, Restart: func() { restarted <- true }})
	mux := http.NewServeMux()
	gateFor(t, mux, srv, "")
	b := &browser{h: mux}

	if w := b.req("POST", "/ui/settings/update/check", url.Values{}); !strings.Contains(w.Header().Get("Location"), "v9.9.9") {
		t.Fatalf("check: %s", w.Header().Get("Location"))
	}
	if home := b.req("GET", "/ui", nil).Body.String(); !strings.Contains(home, "spendbot v9.9.9 is out") {
		t.Error("home banner")
	}
	page := b.req("GET", "/ui/settings", nil).Body.String()
	if !strings.Contains(page, "Install v9.9.9") || !strings.Contains(page, "<li>shiny</li>") || !strings.HasSuffix(strings.TrimSpace(page), "</html>") {
		t.Fatalf("settings:\n%s", tail(page))
	}
	b.req("POST", "/ui/settings/update/apply", url.Values{})
	select {
	case <-restarted:
	case <-time.After(10 * time.Second):
		t.Fatalf("no restart; progress %+v", srv.upd.snapshot())
	}
	if got, _ := os.ReadFile(exe); string(got) != "new program" {
		t.Errorf("program: %q", got)
	}
	if p := srv.upd.snapshot(); p.Done != "v9.9.9" {
		t.Errorf("progress: %+v", p)
	}
}

// A build from source installs a release from the page too; every page marks Settings.
func TestSourceBuildInstalls(t *testing.T) {
	gh := fakeRelease(t, "v9.9.9")
	exe := filepath.Join(t.TempDir(), "spendbot")
	os.WriteFile(exe, []byte("built program"), 0o755)
	st, err := store.Open(context.Background(), t.TempDir()+"/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	up := update.New("v0.5.0-3-gabc-dirty", false, slogDiscard())
	up.Point(gh.URL, func() (string, error) { return exe, nil })
	up.Check(context.Background())
	restarted := make(chan bool, 1)
	srv := New(st, nil, 3, time.Minute, almaty, slogDiscard()).WithSettings(Settings{Updater: up, Restart: func() { restarted <- true }})
	mux := http.NewServeMux()
	gateFor(t, mux, srv, "")
	b := &browser{h: mux}
	page := b.req("GET", "/ui/settings", nil).Body.String()
	if !strings.Contains(page, "Install v9.9.9") || !strings.Contains(page, "built from source") || strings.Contains(page, "git pull") ||
		!strings.Contains(page, "every 5 minutes") {
		t.Fatalf("settings:\n%s", tail(page))
	}
	if ops := b.req("GET", "/ui/operations", nil).Body.String(); !strings.Contains(ops, `title="v9.9.9 is out: install it in Settings">new</span>`) {
		t.Error("the menu does not mark Settings")
	}
	b.req("POST", "/ui/settings/update/apply", url.Values{})
	select {
	case <-restarted:
	case <-time.After(10 * time.Second):
		t.Fatalf("no restart; progress %+v", srv.upd.snapshot())
	}
	if got, _ := os.ReadFile(exe); string(got) != "new program" {
		t.Errorf("program: %q", got)
	}
}

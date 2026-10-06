package update

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		cur, tag string
		want     bool
	}{
		{"v0.4.0", "v0.5.0", true}, {"v0.4.0", "v0.4.0", false}, {"v0.10.0", "v0.9.9", false},
		{"v0.4.0-3-gabc-dirty", "v0.5.0", true}, {"v0.5.0-3-gabc", "v0.5.0", false},
		{"dev", "v0.1.0", true}, {"v0.4.0", "nightly", false}, {"v1.0.0", "v1.0.1", true},
	} {
		if got := Newer(c.cur, c.tag); got != c.want {
			t.Errorf("Newer(%q, %q) = %v", c.cur, c.tag, got)
		}
	}
	if ArchiveName("v0.5.0", "darwin", "arm64") != "spendbot-v0.5.0-macos-arm64.zip" ||
		ArchiveName("v0.5.0", "windows", "amd64") != "spendbot-v0.5.0-windows-amd64.zip" ||
		ArchiveName("v0.5.0", "linux", "amd64") != "spendbot-v0.5.0-linux-amd64.tar.gz" {
		t.Error("archive names must match scripts/dist.sh")
	}
}

// fakeGitHub serves a release whose archive holds prog; sums can be broken on purpose.
func fakeGitHub(t *testing.T, tag string, prog []byte, breakSum bool) *httptest.Server {
	t.Helper()
	name := ArchiveName(tag, "darwin", "arm64")
	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	w, _ := zw.Create("spendbot-" + tag + "-macos-arm64/spendbot")
	w.Write(prog)
	r, _ := zw.Create("spendbot-" + tag + "-macos-arm64/README.txt")
	r.Write([]byte("read me"))
	zw.Close()
	sum := sha256.Sum256(zbuf.Bytes())
	sumHex := hex.EncodeToString(sum[:])
	if breakSum {
		sumHex = strings.Repeat("0", 64)
	}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/" + Repo + "/releases/latest":
			json.NewEncoder(w).Encode(Release{Tag: tag, Name: "spendbot " + tag, Body: "## What's new\n- things",
				Assets: []Asset{{Name: name, URL: srv.URL + "/a/" + name}, {Name: "SHA256SUMS.txt", URL: srv.URL + "/a/sums"}}})
		case "/a/" + name:
			w.Write(zbuf.Bytes())
		case "/a/sums":
			io.WriteString(w, sumHex+"  "+name+"\n")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTest(t *testing.T, current, api string) (*Updater, string) {
	exe := filepath.Join(t.TempDir(), "spendbot")
	os.WriteFile(exe, []byte("old program"), 0o755)
	u := New(current, false, slog.New(slog.NewTextHandler(io.Discard, nil)))
	u.api, u.exe = api, func() (string, error) { return exe, nil }
	return u, exe
}

func TestCheckAndInstall(t *testing.T) {
	srv := fakeGitHub(t, "v0.5.0", []byte("new program"), false)
	u, exe := newTest(t, "v0.4.0", srv.URL)
	if err := u.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := u.Status()
	if !st.Newer || st.Latest.Tag != "v0.5.0" || st.Source {
		t.Fatalf("status: %+v", st)
	}
	rel, _ := u.fetchLatest(context.Background())
	if err := u.install(context.Background(), rel, "darwin", "arm64", exe); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "new program" {
		t.Errorf("program: %q", b)
	}
	if b, _ := os.ReadFile(exe + ".old"); string(b) != "old program" {
		t.Errorf("the old program is kept: %q", b)
	}
	if fi, _ := os.Stat(exe); fi.Mode().Perm()&0o100 == 0 {
		t.Error("the new program must be executable")
	}
}

func TestBadChecksumIsNotInstalled(t *testing.T) {
	srv := fakeGitHub(t, "v0.5.0", []byte("evil program"), true)
	u, exe := newTest(t, "v0.4.0", srv.URL)
	rel, _ := u.fetchLatest(context.Background())
	if err := u.install(context.Background(), rel, "darwin", "arm64", exe); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("want a checksum error: %v", err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "old program" {
		t.Error("the program must stay as it was")
	}
}

func TestSourceBuildIsNotReplaced(t *testing.T) {
	srv := fakeGitHub(t, "v0.5.0", []byte("new"), false)
	u, exe := newTest(t, "v0.4.0-2-gabc-dirty", srv.URL)
	if _, err := u.Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "git pull") {
		t.Fatalf("source build: %v", err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "old program" {
		t.Error("untouched")
	}
	u.Check(context.Background())
	if st := u.Status(); !st.Newer || !st.Source || st.CanApply {
		t.Errorf("a source build still learns about the release: %+v", st)
	}
}

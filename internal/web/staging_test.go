package web

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"spendbot/internal/store"
)

// stageFiles posts files to the uploader like a browser picking several at once.
func stageFiles(t *testing.T, h http.Handler, files map[string][]byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for name, b := range files {
		fw, _ := mw.CreateFormFile("pdf", name)
		fw.Write(b)
	}
	mw.Close()
	r := httptest.NewRequest("POST", "/ui/import/stage", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func stagingServer(t *testing.T) (http.Handler, *store.Store, *Gate) {
	t.Helper()
	st, err := store.Open(context.Background(), t.TempDir()+"/t.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	mux := http.NewServeMux()
	g := gateFor(t, mux, New(st, nil, 3, time.Minute, almaty, slogDiscard()), "")
	return mux, st, g
}

func TestStagingBrokenFile(t *testing.T) {
	h, st, g := stagingServer(t)
	w := stageFiles(t, h, map[string][]byte{"notes.pdf": []byte("not a pdf")})
	if !strings.Contains(w.Header().Get("Location"), "1+statement+added") {
		t.Fatalf("stage: %s", w.Header().Get("Location"))
	}
	b := (&browser{h: h}).req("GET", "/ui/import/staged", nil).Body.String()
	if !strings.Contains(b, "not a readable Kaspi Gold statement") || !strings.Contains(b, "disabled") {
		t.Fatalf("list:\n%s", tail(b))
	}
	if home := (&browser{h: h}).req("GET", "/ui", nil).Body.String(); !strings.Contains(home, "1 statement waits") {
		t.Error("home must say a statement waits")
	}
	files, _ := g.staged(1)
	// a broken file cannot be imported, only removed
	if loc := (&browser{h: h}).req("POST", "/ui/import/staged", url.Values{"action": {"import"}, "id": {files[0].ID}}).Header().Get("Location"); !strings.Contains(loc, "Tick+at+least") {
		t.Errorf("import a broken file: %s", loc)
	}
	if rows, _ := st.Ledger(context.Background(), almaty); len(rows) != 0 {
		t.Error("nothing is imported")
	}
	(&browser{h: h}).req("POST", "/ui/import/staged", url.Values{"action": {"remove"}, "id": {files[0].ID}})
	if files, _ = g.staged(1); len(files) != 0 {
		t.Error("remove")
	}
	if _, err := os.Stat(filepath.Join(g.stageDir(1), "x.pdf")); err == nil {
		t.Error("nothing is left on disk")
	}
}

// Real statements in statements/: collect two, import them oldest first, add one again.
func TestStagingRealStatements(t *testing.T) {
	names, _ := filepath.Glob("../../statements/*.pdf")
	if len(names) < 2 {
		t.Skip("needs two statements in statements/")
	}
	h, st, g := stagingServer(t)
	files := map[string][]byte{}
	for _, n := range names[:2] {
		b, _ := os.ReadFile(n)
		files[filepath.Base(n)] = b
	}
	stageFiles(t, h, files)
	if rows, _ := st.Ledger(context.Background(), almaty); len(rows) != 0 {
		t.Fatal("collecting imports nothing")
	}
	staged, _ := g.staged(1)
	if len(staged) != 2 || staged[0].Ops == 0 || staged[0].From.After(staged[1].From) {
		t.Fatalf("staged, oldest first: %+v", staged)
	}
	page := (&browser{h: h}).req("GET", "/ui/import/staged", nil).Body.String()
	if strings.Count(page, `type="checkbox" name="id"`) != 2 || !strings.Contains(page, "totals match") {
		t.Fatalf("list: ids %d, match %v, differ %v", strings.Count(page, `type="checkbox" name="id"`), strings.Contains(page, "totals match"), strings.Contains(page, "totals differ"))
	}
	w := (&browser{h: h}).req("POST", "/ui/import/staged", url.Values{"action": {"import"}, "id": {staged[0].ID, staged[1].ID}})
	if b := w.Body.String(); w.Code != 200 || !strings.Contains(b, "Statements imported") || strings.Count(b, "operations</span>") != 2 {
		t.Fatalf("results %d:\n%s", w.Code, tail(b))
	}
	if left, _ := g.staged(1); len(left) != 0 {
		t.Error("imported files leave the list")
	}
	if a, _ := g.reg.Get(context.Background(), 1); a.Holder == "" {
		t.Error("the first statement tells whose the account is")
	}
	// the same statement again is marked as imported and not ticked
	b, _ := os.ReadFile(names[0])
	stageFiles(t, h, map[string][]byte{"again.pdf": b})
	page = (&browser{h: h}).req("GET", "/ui/import/staged", nil).Body.String()
	if !strings.Contains(page, "already imported") || strings.Contains(page, `checked  aria-label="Import again.pdf"`) {
		t.Errorf("again:\n%s", tail(page))
	}
}

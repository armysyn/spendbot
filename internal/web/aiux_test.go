package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"spendbot/internal/llm"
	"spendbot/internal/store"
)

func TestAskBoxNeedsAModel(t *testing.T) {
	st, err := store.Open(context.Background(), t.TempDir()+"/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	seedOps(t, st)
	for _, withAI := range []bool{false, true} {
		srv := New(st, nil, 3, time.Minute, almaty, slogDiscard())
		if withAI {
			srv.WithAI(fakeAI{`{}`})
		}
		mux := http.NewServeMux()
		gateFor(t, mux, srv, "")
		for _, p := range []string{"/ui/operations", "/ui/transfers", "/ui/categories?period=all", "/ui/transfers?name=Adam+S."} {
			b := (&browser{h: mux}).req("GET", p, nil).Body.String()
			off := strings.Contains(b, "Connect a model") && strings.Contains(b, `aria-label="Ask in plain words — needs a model"`)
			on := strings.Contains(b, `name="prompt"`)
			if withAI && (!on || off) || !withAI && (on || !off || !strings.Contains(b, "No model is set up.")) {
				t.Errorf("ai=%v %s: on %v off %v", withAI, p, on, off)
			}
		}
	}
}

// fakeOllama streams a slow download; every pull request is counted.
func fakeOllama(t *testing.T) (*httptest.Server, *atomic.Int32) {
	var pulls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			json.NewEncoder(w).Encode(map[string]any{"models": []any{}})
		case "/api/version":
			fmt.Fprint(w, `{"version":"0"}`)
		case "/api/pull":
			n := pulls.Add(1)
			fl := w.(http.Flusher)
			for i := int64(1); ; i++ {
				fmt.Fprintf(w, `{"status":"pulling","completed":%d,"total":1000}`+"\n", i*10*int64(n))
				fl.Flush()
				select {
				case <-r.Context().Done():
					return
				case <-time.After(20 * time.Millisecond):
				}
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &pulls
}

func TestPullPauseResumeCancel(t *testing.T) {
	ol, pulls := fakeOllama(t)
	st, err := store.Open(context.Background(), t.TempDir()+"/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	model := llm.NewClient(ol.URL+"/v1", "qwen2.5:7b", "", slogDiscard())
	srv := New(st, nil, 3, time.Minute, almaty, slogDiscard()).WithSettings(Settings{Model: model, Ollama: llm.NewOllama(ol.URL)})
	mux := http.NewServeMux()
	gateFor(t, mux, srv, "")
	b := &browser{h: mux}
	state := func() pullState {
		var p pullState
		json.Unmarshal(b.req("GET", "/ui/settings/pull", nil).Body.Bytes(), &p)
		return p
	}
	waitFor := func(what string, ok func(pullState) bool) pullState {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			p := state()
			if ok(p) {
				return p
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: %+v", what, p)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	b.req("POST", "/ui/settings/model", url.Values{"model": {"qwen2.5:3b"}})
	waitFor("running", func(p pullState) bool { return p.Running && p.Completed > 0 })

	b.req("POST", "/ui/settings/pull", url.Values{"action": {"pause"}})
	paused := waitFor("paused", func(p pullState) bool { return p.Paused && !p.Running })
	if paused.Err != "" || paused.Model != "qwen2.5:3b" || paused.Completed == 0 {
		t.Errorf("a pause is not an error and keeps the progress: %+v", paused)
	}
	if page := b.req("GET", "/ui/settings", nil).Body.String(); !strings.Contains(page, ">Paused<") || !strings.HasSuffix(strings.TrimSpace(page), "</html>") {
		t.Error("settings page while paused")
	}

	b.req("POST", "/ui/settings/pull", url.Values{"action": {"resume"}})
	waitFor("resumed", func(p pullState) bool { return p.Running && !p.Paused })
	if pulls.Load() != 2 {
		t.Errorf("resume pulls again: %d", pulls.Load())
	}

	b.req("POST", "/ui/settings/pull", url.Values{"action": {"cancel"}})
	waitFor("cancelled", func(p pullState) bool { return !p.Running && !p.Paused && p.Model == "" })
	time.Sleep(100 * time.Millisecond)
	if p := state(); p.Running || p.Err != "" {
		t.Errorf("after cancel: %+v", p)
	}
}

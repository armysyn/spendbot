package web

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"

	"spendbot/internal/llm"
	"spendbot/internal/update"
)

// Settings is what the settings page shows and changes.
type Settings struct {
	ConfigFile  string
	DataDir     string
	IngestURL   string
	IngestToken string
	Telegram    bool
	ClickHouse  bool
	Model       *llm.Client // nil — the local model is off (ANALYSIS_LLM=off)
	Ollama      *llm.Ollama
	Updater     *update.Updater // new releases; nil — not shown
	Restart     func()          // starts the updated program; nil — this account may not update
}

// ModelChoice is a recommended model: the smaller, the weaker the computer it runs on,
// but the rougher the guesses and tips.
type ModelChoice struct {
	Name, Size, Note string
}

var modelChoices = []ModelChoice{
	{"qwen2.5:7b", "4.7 GB", "best guesses; needs a computer with 16 GB of RAM"},
	{"qwen2.5:3b", "1.9 GB", "for 8 GB of RAM; simpler guesses"},
	{"qwen2.5:1.5b", "1 GB", "for a weak computer; rough guesses"},
}

// pullState is a background model download; one at a time.
type pullState struct {
	Model     string `json:"model"`
	Status    string `json:"status"`
	Completed int64  `json:"completed"`
	Total     int64  `json:"total"`
	Running   bool   `json:"running"`
	Err       string `json:"error"`
}

func (s *Server) pullSnapshot() pullState {
	s.pullMu.Lock()
	defer s.pullMu.Unlock()
	return s.pull
}

// WithSettings enables the settings page.
func (s *Server) WithSettings(cfg Settings) *Server {
	s.settings = &cfg
	return s
}

type settingsData struct {
	Page
	Update    *update.Status
	CanUpdate bool
	Updating  progress
	Settings
	OllamaUp  bool
	Installed []string
	Current   string
	Choices   []ModelChoice
	Pull      pullState
	Flash     string
}

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	d := settingsData{Settings: *s.settings, Choices: modelChoices, Pull: s.pullSnapshot(), Flash: r.URL.Query().Get("msg"),
		Update: s.updateStatus(), CanUpdate: s.canUpdate(), Updating: s.upd.snapshot()}
	if d.Model != nil && d.Ollama != nil {
		d.Current = d.Model.Model()
		if d.OllamaUp = d.Ollama.Running(r.Context()); d.OllamaUp {
			d.Installed, _ = d.Ollama.Models(r.Context())
		}
	}
	s.show(w, r, "settings.html", "Settings", "settings", &d)
}

// chooseModel switches the analysis model; if it is missing, it is downloaded in the background first.
func (s *Server) chooseModel(w http.ResponseWriter, r *http.Request) {
	cfg := s.settings
	model := strings.TrimSpace(r.PostFormValue("model"))
	if cfg.Model == nil || cfg.Ollama == nil || model == "" {
		http.Redirect(w, r, "/ui/settings", http.StatusSeeOther)
		return
	}
	installed, err := cfg.Ollama.Models(r.Context())
	if err != nil {
		http.Redirect(w, r, "/ui/settings?msg="+urlq("Ollama does not answer — start it and try again."), http.StatusSeeOther)
		return
	}
	if slices.Contains(installed, model) {
		s.useModel(r.Context(), model)
		http.Redirect(w, r, "/ui/settings?msg="+urlq("Model "+model+" selected."), http.StatusSeeOther)
		return
	}
	s.pullMu.Lock()
	if s.pull.Running {
		s.pullMu.Unlock()
		http.Redirect(w, r, "/ui/settings", http.StatusSeeOther)
		return
	}
	s.pull.Model, s.pull.Status, s.pull.Completed, s.pull.Total, s.pull.Running, s.pull.Err = model, "starting", 0, 0, true, ""
	s.pullMu.Unlock()
	go func() {
		// The download outlives the request: it gets its own context.
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
		defer cancel()
		err := cfg.Ollama.Pull(ctx, model, func(status string, done, total int64) {
			s.pullMu.Lock()
			s.pull.Status = status
			if total > 0 {
				s.pull.Completed, s.pull.Total = done, total
			}
			s.pullMu.Unlock()
		})
		s.pullMu.Lock()
		s.pull.Running = false
		if err != nil {
			s.pull.Err = err.Error()
		} else {
			s.pull.Status = "done"
		}
		s.pullMu.Unlock()
		if err == nil {
			s.useModel(ctx, model)
		} else {
			s.log.Error("web: model pull", "model", model, "err", err)
		}
	}()
	http.Redirect(w, r, "/ui/settings", http.StatusSeeOther)
}

func (s *Server) useModel(ctx context.Context, model string) {
	s.settings.Model.SetModel(model)
	if err := s.st.SetKV(ctx, "analysis_model", model); err != nil {
		s.log.Error("web: save model", "err", err)
	}
	s.log.Info("web: analysis model changed", "model", model)
}

func (s *Server) pullStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.pullSnapshot())
}

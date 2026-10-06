package web

import (
	"context"
	"errors"
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

// pullState is a background model download; one at a time. A paused one keeps its model and
// progress: Ollama keeps the parts it has, so pulling again carries on from there.
type pullState struct {
	Model     string `json:"model"`
	Status    string `json:"status"`
	Completed int64  `json:"completed"`
	Total     int64  `json:"total"`
	Running   bool   `json:"running"`
	Paused    bool   `json:"paused"`
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
	s.startPull(model) // when a download already runs, the page shows it
	http.Redirect(w, r, "/ui/settings#model", http.StatusSeeOther)
}

// startPull downloads a model in the background; false when a download already runs.
func (s *Server) startPull(model string) bool {
	cfg := s.settings
	s.pullMu.Lock()
	if s.pull.Running {
		s.pullMu.Unlock()
		return false
	}
	// carrying on with a paused download keeps its progress on screen
	resume := s.pull.Paused && s.pull.Model == model
	s.pull.Model, s.pull.Running, s.pull.Paused, s.pull.Err = model, true, false, ""
	if !resume {
		s.pull.Status, s.pull.Completed, s.pull.Total = "starting", 0, 0
	} else {
		s.pull.Status = "resuming"
	}
	// The download outlives the request: it gets its own context, which pause and cancel end.
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	s.pullCancel = cancel
	s.pullMu.Unlock()
	go func() {
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
		stopped := ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled)
		s.pull.Running = false
		s.pullCancel = nil
		switch {
		case stopped:
			// paused or cancelled from the page: not an error
		case err != nil:
			s.pull.Err = err.Error()
		default:
			s.pull.Status = "done"
		}
		s.pullMu.Unlock()
		switch {
		case err == nil && !stopped:
			s.useModel(context.Background(), model)
		case err != nil && !stopped:
			s.log.Error("web: model pull", "model", model, "err", err)
		}
	}()
	return true
}

// pullControl pauses, resumes or cancels the model download.
func (s *Server) pullControl(w http.ResponseWriter, r *http.Request) {
	msg := ""
	switch r.PostFormValue("action") {
	case "pause":
		s.pullMu.Lock()
		if s.pull.Running && s.pullCancel != nil {
			s.pull.Paused, s.pull.Status = true, "paused"
			s.pullCancel()
		}
		s.pullMu.Unlock()
		msg = "Download paused. What is downloaded stays; Resume carries on from there."
	case "resume":
		s.pullMu.Lock()
		model, paused := s.pull.Model, s.pull.Paused
		s.pullMu.Unlock()
		if paused && model != "" {
			s.startPull(model)
		}
	case "cancel":
		s.pullMu.Lock()
		if s.pullCancel != nil {
			s.pullCancel()
		}
		model := s.pull.Model
		s.pull = pullState{}
		s.pullMu.Unlock()
		msg = "Download of " + model + " cancelled. Ollama clears the unfinished parts by itself."
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}
	back := "/ui/settings#model"
	if msg != "" {
		back = "/ui/settings?msg=" + urlq(msg) + "#model"
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
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

package web

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"spendbot/internal/accounts"
	"spendbot/internal/importer"
	"spendbot/internal/statement"
)

// Statements are collected first and imported on a button press: several files at once are
// parsed for a preview — card, period, operations, totals, whose — and wait on disk in the
// account's staging folder (a week at most) until the person imports or removes them.

const (
	maxStaged     = 40
	maxBatchBytes = 200 << 20
	stagedTTL     = 7 * 24 * time.Hour
)

// stagedFile is a statement waiting to be imported. What is parsed at upload is kept; whether
// it is already imported or another person's is worked out when the list is shown.
type stagedFile struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Size     int       `json:"size"`
	Added    time.Time `json:"added"`
	Account  string    `json:"account"`
	Holder   string    `json:"holder"`
	From     time.Time `json:"from"`
	To       time.Time `json:"to"`
	Ops      int       `json:"ops"`
	Verify   string    `json:"verify"` // the totals do not match: why
	ParseErr string    `json:"parse_error"`

	Known bool `json:"-"` // a statement for this card and period is already imported
	Other bool `json:"-"` // another person's statement
}

func (g *Gate) stageDir(accountID int64) string {
	return filepath.Join(g.uploads, "staged", strconv.FormatInt(accountID, 10))
}

func validID(id string) bool {
	return len(id) == 32 && strings.Trim(id, "0123456789abcdef") == ""
}

// stage keeps a file with its preview.
func (g *Gate) stage(accountID int64, f stagedFile, b []byte) error {
	dir := g.stageDir(accountID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	meta, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, f.ID+".pdf"), b, 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, f.ID+".json"), meta, 0o600)
}

// staged lists the files waiting, the oldest period first; ones older than a week are removed.
func (g *Gate) staged(accountID int64) ([]stagedFile, error) {
	dir := g.stageDir(accountID)
	metas, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	var out []stagedFile
	for _, m := range metas {
		b, err := os.ReadFile(m)
		if err != nil {
			continue
		}
		var f stagedFile
		if json.Unmarshal(b, &f) != nil || !validID(f.ID) {
			continue
		}
		if g.now().Sub(f.Added) > stagedTTL {
			g.unstage(accountID, f.ID)
			continue
		}
		out = append(out, f)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].From.Equal(out[j].From) {
			return out[i].From.Before(out[j].From)
		}
		return out[i].Added.Before(out[j].Added)
	})
	return out, nil
}

func (g *Gate) stagedBytes(accountID int64, id string) ([]byte, error) {
	if !validID(id) {
		return nil, errors.New("no such file")
	}
	return os.ReadFile(filepath.Join(g.stageDir(accountID), id+".pdf"))
}

func (g *Gate) unstage(accountID int64, id string) {
	if !validID(id) {
		return
	}
	for _, ext := range []string{".pdf", ".json"} {
		if err := os.Remove(filepath.Join(g.stageDir(accountID), id+ext)); err != nil && !errors.Is(err, os.ErrNotExist) {
			g.log.Warn("web: remove a staged file", "err", err)
		}
	}
}

func newID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// preview parses a statement for the list without importing it.
func (s *Server) preview(name string, b []byte, now time.Time) stagedFile {
	f := stagedFile{Name: name, Size: len(b), Added: now}
	st, err := statement.ParseKaspiBytes(b, s.loc)
	if err != nil {
		f.ParseErr = err.Error()
		return f
	}
	f.Account, f.Holder, f.From, f.To, f.Ops = st.Account, st.Holder, st.From, st.To, len(st.Ops)
	if err := st.Verify(); err != nil {
		f.Verify = err.Error()
	}
	return f
}

// stageUpload takes one or more PDF files and adds them to the list.
func (s *Server) stageUpload(w http.ResponseWriter, r *http.Request) {
	v := viewerFrom(r.Context())
	if s.gate == nil || v.Account.ID == 0 {
		http.NotFound(w, r)
		return
	}
	back := func(msg string) { http.Redirect(w, r, "/ui/import/staged?msg="+urlq(msg), http.StatusSeeOther) }
	r.Body = http.MaxBytesReader(w, r.Body, maxBatchBytes)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		back("The files are too large together (up to 200 MB at once); add them in parts.")
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()
	files := r.MultipartForm.File["pdf"]
	if len(files) == 0 {
		back("Choose one or more PDF statements.")
		return
	}
	have, err := s.gate.staged(v.Account.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	added, skipped := 0, 0
	for _, fh := range files {
		if len(have)+added >= maxStaged {
			skipped++
			continue
		}
		if fh.Size > maxUpload {
			skipped++
			continue
		}
		fr, err := fh.Open()
		if err != nil {
			skipped++
			continue
		}
		b, err := io.ReadAll(fr)
		fr.Close()
		if err != nil {
			skipped++
			continue
		}
		f := s.preview(trimTo(filepath.Base(fh.Filename), 120), b, s.now())
		if f.ID, err = newID(); err != nil {
			s.fail(w, err)
			return
		}
		if err := s.gate.stage(v.Account.ID, f, b); err != nil {
			s.fail(w, err)
			return
		}
		added++
	}
	msg := fmt.Sprintf("%d %s added — check them and press Import.", added, plural(added, "statement", "statements"))
	if skipped > 0 {
		msg += fmt.Sprintf(" %d skipped (over 20 MB, unreadable, or over %d waiting).", skipped, maxStaged)
	}
	back(msg)
}

type stagedData struct {
	Page
	Flash     string
	Files     []stagedFile
	Ready     int // can be imported
	Mine      string
	CanCreate bool
	Results   []stagedResult
	NewFor    *stagedFile // the file a new account is being made for
}

type stagedResult struct {
	Name   string
	From   time.Time
	To     time.Time
	Result importer.Result
	Err    string
}

// stagedPage lists the files waiting with their previews.
func (s *Server) stagedPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	v := viewerFrom(ctx)
	if s.gate == nil || v.Account.ID == 0 {
		http.NotFound(w, r)
		return
	}
	d := &stagedData{Flash: r.URL.Query().Get("msg"), Mine: v.Account.Holder, CanCreate: v.Account.Hash != "" || !v.Open}
	var err error
	if d.Files, err = s.gate.staged(v.Account.ID); err != nil {
		s.fail(w, err)
		return
	}
	known, err := s.st.Statements(ctx, s.loc)
	if err != nil {
		s.fail(w, err)
		return
	}
	for i := range d.Files {
		f := &d.Files[i]
		for _, k := range known {
			if k.Account == f.Account && k.From.Equal(f.From) && k.To.Equal(f.To) && k.Ops >= f.Ops {
				f.Known = true
			}
		}
		f.Other = f.Holder != "" && v.Account.Holder != "" && !accounts.SameHolder(f.Holder, v.Account.Holder)
		if f.ParseErr == "" {
			d.Ready++
		}
		if f.ID == r.URL.Query().Get("new") && d.CanCreate {
			d.NewFor = f
		}
	}
	s.show(w, r, "staged.html", "Statements to import", "home", d)
}

// stagedAction imports the chosen files, oldest period first, or removes one or all.
func (s *Server) stagedAction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	v := viewerFrom(ctx)
	if s.gate == nil || v.Account.ID == 0 {
		http.NotFound(w, r)
		return
	}
	back := func(msg string) { http.Redirect(w, r, "/ui/import/staged?msg="+urlq(msg), http.StatusSeeOther) }
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	files, err := s.gate.staged(v.Account.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	switch r.PostFormValue("action") {
	case "remove":
		s.gate.unstage(v.Account.ID, r.PostFormValue("id"))
		back("Removed from the list.")
	case "clear":
		for _, f := range files {
			s.gate.unstage(v.Account.ID, f.ID)
		}
		back("The list is empty.")
	case "import":
		chosen := map[string]bool{}
		for _, id := range r.PostForm["id"] {
			chosen[id] = true
		}
		d := &stagedData{Mine: v.Account.Holder}
		holder := v.Account.Holder
		for _, f := range files { // already the oldest period first
			if !chosen[f.ID] || f.ParseErr != "" {
				continue
			}
			res := stagedResult{Name: f.Name, From: f.From, To: f.To}
			b, err := s.gate.stagedBytes(v.Account.ID, f.ID)
			if err != nil {
				res.Err = "the file is gone; add it again"
				d.Results = append(d.Results, res)
				continue
			}
			st, err := statement.ParseKaspiBytes(b, s.loc)
			if err != nil {
				res.Err = err.Error()
				d.Results = append(d.Results, res)
				continue
			}
			if res.Result, err = importer.Import(ctx, s.st, st, s.now()); err != nil {
				s.fail(w, err)
				return
			}
			// the first statement of an account tells whose it is
			if holder == "" && st.Holder != "" {
				holder = st.Holder
				if err := s.gate.SetHolder(ctx, v.Account.ID, holder); err != nil {
					s.fail(w, err)
					return
				}
			}
			s.gate.unstage(v.Account.ID, f.ID)
			d.Results = append(d.Results, res)
		}
		if len(d.Results) == 0 {
			back("Tick at least one statement to import.")
			return
		}
		s.log.Info("web: statements imported", "files", len(d.Results))
		if s.kick != nil {
			s.kick.Kick()
		}
		if d.Files, err = s.gate.staged(v.Account.ID); err != nil {
			s.fail(w, err)
			return
		}
		s.show(w, r, "staged.html", "Statements imported", "home", d)
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
	}
}

// stagedCount is how many statements wait, for the home page.
func (s *Server) stagedCount(r *http.Request) int {
	v := viewerFrom(r.Context())
	if s.gate == nil || v.Account.ID == 0 {
		return 0
	}
	fs, err := s.gate.staged(v.Account.ID)
	if err != nil {
		return 0
	}
	return len(fs)
}

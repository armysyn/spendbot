package web

import (
	"context"
	"encoding/json"
	"html/template"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"spendbot/internal/update"
)

// Updates from the settings page: check GitHub now, and install the latest release in the
// background — the page follows the progress and reloads when the new program answers.

// progress is how an update from the page goes.
type progress struct {
	Running bool   `json:"running"`
	Done    string `json:"done"` // the tag installed; the program restarts
	Err     string `json:"error"`
}

type updateState struct {
	mu sync.Mutex
	p  progress
}

func (u *updateState) snapshot() progress {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.p
}

func (s *Server) canUpdate() bool {
	return s.settings != nil && s.settings.Updater != nil && s.settings.Restart != nil
}

// updateStatus is what the pages show about releases; nil — nothing to show.
func (s *Server) updateStatus() *update.Status {
	if s.settings == nil || s.settings.Updater == nil {
		return nil
	}
	st := s.settings.Updater.Status()
	return &st
}

func (s *Server) checkUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.canUpdate() {
		http.NotFound(w, r)
		return
	}
	msg := "Checked: this is the latest spendbot."
	if err := s.settings.Updater.Check(r.Context()); err != nil {
		msg = "Could not check: " + err.Error()
	} else if st := s.settings.Updater.Status(); st.Newer {
		msg = st.Latest.Tag + " is available."
	}
	http.Redirect(w, r, "/ui/settings?msg="+urlq(msg)+"#updates", http.StatusSeeOther)
}

func (s *Server) applyUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.canUpdate() {
		http.NotFound(w, r)
		return
	}
	s.upd.mu.Lock()
	if s.upd.p.Running {
		s.upd.mu.Unlock()
		http.Redirect(w, r, "/ui/settings#updates", http.StatusSeeOther)
		return
	}
	s.upd.p = progress{Running: true}
	s.upd.mu.Unlock()
	go func() {
		// the download outlives the request
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		tag, err := s.settings.Updater.Apply(ctx)
		s.upd.mu.Lock()
		s.upd.p.Running = false
		if err != nil {
			s.upd.p.Err = err.Error()
			s.upd.mu.Unlock()
			s.log.Error("web: update", "err", err)
			return
		}
		s.upd.p.Done = tag
		s.upd.mu.Unlock()
		time.Sleep(1500 * time.Millisecond) // let the page see "installed"
		s.settings.Restart()
	}()
	http.Redirect(w, r, "/ui/settings#updates", http.StatusSeeOther)
}

func (s *Server) updateProgress(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.upd.snapshot())
}

var (
	mdBold = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	mdCode = regexp.MustCompile("`([^`]+)`")
	mdLink = regexp.MustCompile(`\[([^\]]+)\]\((https?://[^)\s]+)\)`)
)

// notesHTML shows release notes: the little Markdown they use — headings, lists, tables,
// bold, code and links — over escaped text, so nothing in them runs on the page.
func notesHTML(md string) template.HTML {
	inline := func(s string) string {
		s = template.HTMLEscapeString(s)
		s = mdLink.ReplaceAllString(s, `<a href="$2" target="_blank" rel="noopener">$1</a>`)
		s = mdBold.ReplaceAllString(s, "<b>$1</b>")
		return mdCode.ReplaceAllString(s, "<code>$1</code>")
	}
	var out strings.Builder
	list := ""
	closeList := func() {
		if list != "" {
			out.WriteString("</" + list + ">")
			list = ""
		}
	}
	lines := strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n")
	for i := 0; i < len(lines); i++ {
		l := strings.TrimRight(lines[i], " ")
		t := strings.TrimSpace(l)
		switch {
		case t == "":
			closeList()
		case strings.HasPrefix(t, "#"):
			closeList()
			out.WriteString("<h4>" + inline(strings.TrimLeft(t, "# ")) + "</h4>")
		case strings.HasPrefix(t, "|"):
			closeList()
			out.WriteString(`<table class="data">`)
			for ; i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), "|"); i++ {
				row := strings.Trim(strings.TrimSpace(lines[i]), "|")
				if strings.Trim(row, "-|: ") == "" {
					continue // the separator under the header
				}
				out.WriteString("<tr>")
				for _, c := range strings.Split(row, "|") {
					out.WriteString("<td>" + inline(strings.TrimSpace(c)) + "</td>")
				}
				out.WriteString("</tr>")
			}
			i--
			out.WriteString("</table>")
		case strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* "):
			if list != "ul" {
				closeList()
				out.WriteString("<ul>")
				list = "ul"
			}
			out.WriteString("<li>" + inline(t[2:]) + "</li>")
		case len(t) > 2 && t[0] >= '0' && t[0] <= '9' && strings.Contains(t[:min(4, len(t))], ". "):
			if list != "ol" {
				closeList()
				out.WriteString("<ol>")
				list = "ol"
			}
			out.WriteString("<li>" + inline(t[strings.Index(t, ". ")+2:]) + "</li>")
		default:
			closeList()
			out.WriteString("<p>" + inline(t) + "</p>")
		}
	}
	closeList()
	return template.HTML(out.String()) //nolint:gosec // built from escaped text above
}

package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"spendbot/internal/store"
)

// Issues: problems and wishes about spendbot itself, kept in the local database until GitHub
// issues are set up.

type issuesData struct {
	Page
	Flash        string
	Issues       []store.Issue
	Status, Kind string
	Priority     string
	Query        string
	OpenN        int
	ClosedN      int
	New          bool   // the form for a new issue is open
	NewPage      string // the page the new issue is about
	State        string
}

func (s *Server) issues(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	d := &issuesData{Flash: q.Get("msg"), Status: q.Get("status"), Kind: q.Get("kind"), Priority: q.Get("priority"),
		Query: strings.TrimSpace(q.Get("q")), New: q.Get("new") == "1", NewPage: safeBack(q.Get("page"), "")}
	switch d.Status {
	case store.IssueOpen, store.IssueClosed, "all":
	default:
		d.Status = store.IssueOpen
	}
	f := store.IssueFilter{Status: d.Status, Kind: d.Kind, Priority: d.Priority, Query: d.Query}
	if f.Status == "all" {
		f.Status = ""
	}
	var err error
	if d.Issues, err = s.st.Issues(ctx, f); err != nil {
		s.fail(w, err)
		return
	}
	if d.OpenN, d.ClosedN, err = s.st.IssueCounts(ctx); err != nil {
		s.fail(w, err)
		return
	}
	sq := cloneValues(q)
	for _, k := range []string{"msg", "new", "page"} {
		sq.Del(k)
	}
	d.State = sq.Encode()
	s.show(w, r, "issues.html", "Issues", "issues", d)
}

// issueFrom reads and checks the fields of the issue form.
func issueFrom(r *http.Request) (store.Issue, string) {
	is := store.Issue{
		Kind:     r.PostFormValue("kind"),
		Title:    trimTo(r.PostFormValue("title"), 140),
		Body:     strings.TrimSpace(r.PostFormValue("body")),
		Priority: r.PostFormValue("priority"),
		Page:     safeBack(strings.TrimSpace(r.PostFormValue("page")), ""),
	}
	if is.Kind != store.IssueBug {
		is.Kind = store.IssueIdea
	}
	switch is.Priority {
	case store.PriorityLow, store.PriorityHigh:
	default:
		is.Priority = store.PriorityNormal
	}
	if len([]rune(is.Body)) > 10000 {
		is.Body = string([]rune(is.Body)[:10000])
	}
	if is.Title == "" {
		return is, "Give it a title: what is wrong, or what you wish for."
	}
	return is, ""
}

func (s *Server) createIssue(w http.ResponseWriter, r *http.Request) {
	is, problem := issueFrom(r)
	if problem != "" {
		http.Redirect(w, r, "/ui/issues?new=1&msg="+urlq(problem), http.StatusSeeOther)
		return
	}
	id, err := s.st.SaveIssue(r.Context(), is, s.now())
	if err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/ui/issues/"+strconv.FormatInt(id, 10)+"?msg="+urlq("Saved. It stays on this computer until GitHub issues are set up."), http.StatusSeeOther)
}

type issueData struct {
	Page
	Flash    string
	Issue    store.Issue
	Comments []store.IssueComment
}

func (s *Server) issue(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	d := &issueData{Flash: r.URL.Query().Get("msg")}
	if d.Issue, err = s.st.Issue(ctx, id); errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		s.fail(w, err)
		return
	}
	if d.Comments, err = s.st.IssueComments(ctx, id); err != nil {
		s.fail(w, err)
		return
	}
	s.show(w, r, "issue.html", "#"+strconv.FormatInt(id, 10)+" "+d.Issue.Title, "issues", d)
}

// issueAction edits, comments on, closes, reopens and deletes an issue.
func (s *Server) issueAction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if _, err := s.st.Issue(ctx, id); errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		s.fail(w, err)
		return
	}
	self := "/ui/issues/" + strconv.FormatInt(id, 10)
	back := func(path, msg string) { http.Redirect(w, r, path+"?msg="+urlq(msg), http.StatusSeeOther) }
	now := s.now()
	switch r.PostFormValue("action") {
	case "edit":
		is, problem := issueFrom(r)
		if problem != "" {
			back(self, problem)
			return
		}
		is.ID = id
		if _, err := s.st.SaveIssue(ctx, is, now); err != nil {
			s.fail(w, err)
			return
		}
		back(self, "Saved.")
	case "comment":
		body := strings.TrimSpace(r.PostFormValue("body"))
		if body == "" {
			back(self, "The comment is empty.")
			return
		}
		if len([]rune(body)) > 10000 {
			body = string([]rune(body)[:10000])
		}
		if err := s.st.AddIssueComment(ctx, id, body, now); err != nil {
			s.fail(w, err)
			return
		}
		if r.PostFormValue("close") == "1" {
			if err := s.st.SetIssueStatus(ctx, id, store.IssueClosed, now); err != nil {
				s.fail(w, err)
				return
			}
			back(self, "Commented and closed.")
			return
		}
		back(self, "Comment added.")
	case "close", "reopen":
		status, msg := store.IssueClosed, "Closed."
		if r.PostFormValue("action") == "reopen" {
			status, msg = store.IssueOpen, "Reopened."
		}
		if err := s.st.SetIssueStatus(ctx, id, status, now); err != nil {
			s.fail(w, err)
			return
		}
		back(self, msg)
	case "delete":
		if err := s.st.DeleteIssue(ctx, id); err != nil {
			s.fail(w, err)
			return
		}
		back("/ui/issues", "Issue deleted.")
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
	}
}

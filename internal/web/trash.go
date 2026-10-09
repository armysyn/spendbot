package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"spendbot/internal/analytics"
	"spendbot/internal/kaspi"
	"spendbot/internal/store"
)

// Deleting operations: one from its ✎ menu or several picked on the operations page. They go
// to the trash, from where they can be restored; the message after deleting offers Undo.

func formIDs(r *http.Request) []int64 {
	if err := r.ParseForm(); err != nil {
		return nil
	}
	var ids []int64
	for _, v := range r.PostForm["id"] {
		for _, p := range strings.Split(v, ",") {
			if id, err := strconv.ParseInt(strings.TrimSpace(p), 10, 64); err == nil && id > 0 {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

func joinIDs(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, ",")
}

// deleteOps moves the posted operations (id, repeated or comma-separated) to the trash.
func (s *Server) deleteOps(w http.ResponseWriter, r *http.Request) {
	ids := formIDs(r)
	back := safeBack(r.PostFormValue("back"), "/ui/operations")
	if len(ids) == 0 {
		http.Redirect(w, r, withMsg(back, "Pick the operations to delete."), http.StatusSeeOther)
		return
	}
	trashed, err := s.st.TrashTxs(r.Context(), ids, s.now())
	if err != nil {
		s.fail(w, err)
		return
	}
	if s.kick != nil {
		s.kick.Kick()
	}
	n := len(trashed)
	u := withMsg(back, fmt.Sprintf("%d %s deleted.", n, plural(n, "operation", "operations")))
	if n > 0 {
		u = withParam(u, "undo", joinIDs(trashed))
	}
	http.Redirect(w, r, u, http.StatusSeeOther)
}

// restoreOps brings operations back from the trash (id — trash ids).
func (s *Server) restoreOps(w http.ResponseWriter, r *http.Request) {
	ids := formIDs(r)
	back := safeBack(r.PostFormValue("back"), "/ui/trash")
	n, err := s.st.RestoreTrash(r.Context(), ids)
	if err != nil {
		s.fail(w, err)
		return
	}
	if s.kick != nil {
		s.kick.Kick()
	}
	http.Redirect(w, r, withMsg(back, fmt.Sprintf("%d %s restored.", n, plural(n, "operation", "operations"))), http.StatusSeeOther)
}

type trashData struct {
	Page
	Flash string
	Items []trashRow
}

type trashRow struct {
	store.Trashed
	Title string // as the operations page names it
}

func (s *Server) trash(w http.ResponseWriter, r *http.Request) {
	d := &trashData{Flash: r.URL.Query().Get("msg")}
	items, err := s.st.Trash(r.Context(), s.loc)
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, t := range items {
		title := displayName(t.MerchantRaw)
		switch {
		case t.Kind == kaspi.Withdrawal:
			title = analytics.CashMerchant
		case t.MerchantRaw == "" && t.Kind != "":
			title = kaspi.KindName(t.Kind)
		}
		d.Items = append(d.Items, trashRow{Trashed: t, Title: title})
	}
	s.show(w, r, "trash.html", "Deleted operations", "operations", d)
}

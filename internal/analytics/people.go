package analytics

import (
	"sort"
	"strings"
	"time"

	"spendbot/internal/kaspi"
	"spendbot/internal/store"
)

// Person is someone money was sent to or received from over a period. Kaspi statements show
// only a first name and the initial of the surname, so namesakes with one initial merge.
type Person struct {
	Name        string
	Sent        int64 // transfers to them, tiyn
	SentN       int
	Received    int64 // top-ups from them, tiyn
	ReceivedN   int
	First, Last time.Time // within the period
	FirstEver   time.Time // the first operation with them over all time
}

// Balance is what came from them minus what went to them.
func (p Person) Balance() int64 { return p.Received - p.Sent }

// PeopleFilter selects people for the transfers page. Amounts and counts are totals per person
// over the period, on the chosen side: what was sent to them, or with Dir == DirIn, what came
// from them.
type PeopleFilter struct {
	Period   Period
	Dir      string // DirOut — people money was sent to, DirIn — people money came from
	Query    string
	Min, Max int64 // total on the side, tiyn; 0 — no bound
	TimesMin int   // number of operations on the side; 0 — no bound
	TimesMax int
	New      bool   // the first operation with them ever falls inside the period
	Sort     string // see SortPeople
}

// People aggregates transfers and top-ups per person.
func People(all []store.LedgerRow, f PeopleFilter) []Person {
	by := map[string]*Person{}
	var order []string
	for _, r := range all {
		if r.Currency != "KZT" || r.Merchant == "" || r.Kind != transfer && r.Kind != kaspi.TopUp {
			continue
		}
		p, ok := by[r.Merchant]
		if !ok {
			p = &Person{Name: r.Merchant, FirstEver: r.At}
			by[r.Merchant] = p
			order = append(order, r.Merchant)
		}
		if r.At.Before(p.FirstEver) {
			p.FirstEver = r.At
		}
		if !f.Period.contains(r.At) {
			continue
		}
		if r.Kind == transfer {
			p.Sent += r.Amount
			p.SentN++
		} else {
			p.Received -= r.Amount
			p.ReceivedN++
		}
		if p.First.IsZero() || r.At.Before(p.First) {
			p.First = r.At
		}
		if r.At.After(p.Last) {
			p.Last = r.At
		}
	}
	terms := strings.Fields(Fold(f.Query))
	var out []Person
	for _, name := range order {
		p := *by[name]
		total, n := p.Sent, p.SentN
		if f.Dir == DirIn {
			total, n = p.Received, p.ReceivedN
		}
		switch {
		case p.SentN+p.ReceivedN == 0:
			continue
		case f.Dir != "" && n == 0:
			continue
		case f.Min > 0 && total < f.Min, f.Max > 0 && total > f.Max:
			continue
		case f.TimesMin > 0 && n < f.TimesMin, f.TimesMax > 0 && n > f.TimesMax:
			continue
		case f.New && !f.Period.contains(p.FirstEver):
			continue
		}
		if len(terms) > 0 {
			hay := Fold(p.Name)
			match := true
			for _, t := range terms {
				match = match && strings.Contains(hay, t)
			}
			if !match {
				continue
			}
		}
		out = append(out, p)
	}
	SortPeople(out, f.Sort, f.Dir)
	return out
}

// Sorts for SortPeople; the default is by turnover, money both ways.
const (
	PeopleTurnover = ""
	PeopleBig      = "big"     // the largest total on the side first
	PeopleSmall    = "small"   // the smallest total on the side first
	PeopleCount    = "count"   // the most operations on the side first
	PeopleNew      = "new"     // the latest operation first
	PeopleOld      = "old"     // the earliest operation first
	PeopleBalance  = "balance" // received minus sent, the most in their favour first
	PeopleName     = "name"
)

// SortPeople orders people; "the side" is what was sent, or what came in with dir == DirIn.
func SortPeople(ps []Person, by, dir string) {
	side := func(p Person) (int64, int) {
		if dir == DirIn {
			return p.Received, p.ReceivedN
		}
		return p.Sent, p.SentN
	}
	sort.SliceStable(ps, func(i, j int) bool {
		a, b := ps[i], ps[j]
		sa, na := side(a)
		sb, nb := side(b)
		switch by {
		case PeopleBig:
			if sa != sb {
				return sa > sb
			}
		case PeopleSmall:
			if sa != sb {
				return sa < sb
			}
		case PeopleCount:
			if na != nb {
				return na > nb
			}
		case PeopleNew:
			if !a.Last.Equal(b.Last) {
				return a.Last.After(b.Last)
			}
		case PeopleOld:
			if !a.First.Equal(b.First) {
				return a.First.Before(b.First)
			}
		case PeopleBalance:
			if a.Balance() != b.Balance() {
				return a.Balance() < b.Balance()
			}
		case PeopleName:
			return a.Name < b.Name
		}
		ta, tb := a.Sent+a.Received, b.Sent+b.Received
		if ta != tb {
			return ta > tb
		}
		return a.Name < b.Name
	})
}

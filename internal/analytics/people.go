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
// Sums and counts cover the operations that matched the filter.
type Person struct {
	Name        string
	Sent        int64 // transfers to them, tiyn
	SentN       int
	Received    int64 // top-ups from them, tiyn
	ReceivedN   int
	First, Last time.Time // of the matching operations
	FirstEver   time.Time // the first operation with them over all time
	allOut      int       // every transfer to them in the period, matching or not
	allIn       int
}

// Balance is what came from them minus what went to them.
func (p Person) Balance() int64 { return p.Received - p.Sent }

// PeopleFilter selects people and their operations for the transfers page.
type PeopleFilter struct {
	Period Period
	Dir    string // DirOut — money sent to them, DirIn — money from them; empty — both
	Query  string
	Name   string // exactly this person
	// Min and Max bound each operation, without its sign: a person shows up with the
	// operations inside the range, and their sums cover only those.
	Min, Max int64
	// TimesMin and TimesMax bound how many operations there were with a person in the period
	// on the chosen side (sent, or received with DirIn) — all of them, not only the matching
	// ones: "sent once, between 20k and 50k" means one transfer at all, and it is in the range.
	TimesMin, TimesMax int
	New                bool   // the first operation with them ever falls inside the period
	Sort               string // see SortPeople
}

// PeopleResult is the people that match and their matching operations, newest first.
type PeopleResult struct {
	People []Person
	Ops    []Op
}

// People aggregates transfers and top-ups per person.
func People(all []store.LedgerRow, f PeopleFilter) PeopleResult {
	by := map[string]*Person{}
	var order []string
	opsBy := map[string][]Op{}
	of := Filter{Min: f.Min, Max: f.Max, Dir: f.Dir}
	for _, r := range all {
		if r.Currency != "KZT" || r.Merchant == "" || r.Kind != transfer && r.Kind != kaspi.TopUp {
			continue
		}
		if f.Name != "" && r.Merchant != f.Name {
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
			p.allOut++
		} else {
			p.allIn++
		}
		if !inRange(r.Amount, of) {
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
		opsBy[r.Merchant] = append(opsBy[r.Merchant], Op{TxID: r.TxID, At: r.At, Merchant: r.Merchant, MerchantKey: r.MerchantNorm,
			Kind: r.Kind, Status: r.Status, Source: r.Source, Amount: r.Amount, Full: r.Amount, Categories: r.Categories,
			Spend: IsSpend(r)})
	}
	terms := strings.Fields(Fold(f.Query))
	var res PeopleResult
	for _, name := range order {
		p := *by[name]
		n := p.allOut
		if f.Dir == DirIn {
			n = p.allIn
		}
		switch {
		case p.SentN+p.ReceivedN == 0:
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
		res.People = append(res.People, p)
		res.Ops = append(res.Ops, opsBy[name]...)
	}
	SortPeople(res.People, f.Sort, f.Dir)
	switch f.Sort {
	case PeopleBig, PeopleSmall, PeopleOld:
		SortOps(res.Ops, f.Sort) // the same keys: big, small, old
	default:
		SortOps(res.Ops, SortNew)
	}
	return res
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

package analytics

// Financial independence, the rough way: a capital of 25 years of spending lets one live on
// what it earns (the 4% rule). Each year the capital grows by a real return — over inflation —
// and the year's savings are added; the years until it reaches the target are counted.

const (
	FIMultiple   = 25   // years of spending to have: 1 / 4%
	FIRealReturn = 0.05 // a cautious real return of a diversified portfolio
	fiMaxYears   = 100
)

// FIRow is the years to independence at a savings rate.
type FIRow struct {
	Rate    int // %
	Years   int
	Never   bool
	Current bool // the rate closest to the person's own
}

type FI struct {
	Spend    int64 // a year of what went out
	Save     int64 // a year of savings: income − went out
	Income   int64
	Capital  int64 // net worth now
	Target   int64 // Spend × 25
	Rate     float64
	Years    int
	Never    bool // nothing is saved: the target is not reached
	Done     bool // already there
	Progress float64
	Table    []FIRow
}

// YearsToFI counts whole years until capital, growing by r and topped up with save each year,
// reaches target; -1 — never within a hundred years.
func YearsToFI(capital, save, target int64, r float64) int {
	if capital >= target {
		return 0
	}
	c := float64(capital)
	for y := 1; y <= fiMaxYears; y++ {
		c = c*(1+r) + float64(save)
		if c >= float64(target) {
			return y
		}
	}
	return -1
}

// BuildFI estimates the years to independence from a year of income and spending and the
// capital now, with a table of other savings rates at the same income.
func BuildFI(income, spend, capital int64) FI {
	f := FI{Income: income, Spend: spend, Save: income - spend, Capital: capital, Target: spend * FIMultiple}
	if income > 0 {
		f.Rate = float64(f.Save) * 100 / float64(income)
	}
	if f.Target > 0 {
		f.Progress = float64(max(capital, 0)) * 100 / float64(f.Target)
	}
	switch y := YearsToFI(capital, f.Save, f.Target, FIRealReturn); {
	case f.Target <= 0:
	case y == 0:
		f.Done = true
	case y < 0:
		f.Never = true
	default:
		f.Years = y
	}
	if income <= 0 {
		return f
	}
	closest := -1
	for i, rate := range []int{5, 10, 15, 20, 25, 30, 40, 50, 60, 70} {
		save := income * int64(rate) / 100
		spend := income - save
		y := YearsToFI(capital, save, spend*FIMultiple, FIRealReturn)
		f.Table = append(f.Table, FIRow{Rate: rate, Years: y, Never: y < 0})
		if closest < 0 || abs64f(float64(rate)-f.Rate) < abs64f(float64(f.Table[closest].Rate)-f.Rate) {
			closest = i
		}
	}
	if f.Rate > 0 {
		f.Table[closest].Current = true
	}
	return f
}

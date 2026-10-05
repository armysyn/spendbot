// Package kaspi holds the Russian labels Kaspi Bank prints in Kaspi Gold statements.
// They are data the parser and the database match on, not UI text; every label has an
// English name for display.
package kaspi

// Operation kinds as they appear in the "Операция" column of a statement.
const (
	Purchase    = "Покупка"
	Withdrawal  = "Снятие"
	Transfer    = "Перевод"
	TopUp       = "Пополнение"
	OwnTransfer = "Перевод на свой счет"
	FromOwn     = "Поступление со своего счета"
	Other       = "Разное"
)

// Summary is a total from the statement header and the operation kind it adds up.
type Summary struct {
	Label   string // as printed in the header, e.g. "Покупки"
	Kind    string
	English string
}

// Summaries lists the header totals that are checked against parsed operations.
var Summaries = []Summary{
	{"Покупки", Purchase, "Purchases"},
	{"Снятия", Withdrawal, "Cash withdrawals"},
	{"Переводы", Transfer, "Transfers"},
	{"Переводы на свои счета", OwnTransfer, "Transfers to own accounts"},
	{"Пополнения", TopUp, "Top-ups"},
	{"Поступления со своих счетов", FromOwn, "From own accounts"},
	{"Разное", Other, "Other"},
}

var kindNames = map[string]string{
	Purchase: "Purchase", Withdrawal: "Cash withdrawal", Transfer: "Transfer", TopUp: "Top-up",
	OwnTransfer: "Transfer to own account", FromOwn: "From own account", Other: "Other",
}

// KindName is the English name of an operation kind; unknown kinds are returned as is.
func KindName(kind string) string {
	if n, ok := kindNames[kind]; ok {
		return n
	}
	return kind
}

// SummaryName is the English name of a header total.
func SummaryName(label string) string {
	for _, s := range Summaries {
		if s.Label == label {
			return s.English
		}
	}
	return label
}

// Labels of the operations table header.
const (
	ColDate    = "Дата"     // date
	ColAmount  = "Сумма"    // amount
	ColKind    = "Операция" // operation
	ColDetails = "Детали"   // details
)

// PeriodPattern matches the statement period line, "за период с 05.10.25 по 05.10.26"
// ("for the period from … to …").
const PeriodPattern = `за период с (\d{2}\.\d{2}\.\d{2}) по (\d{2}\.\d{2}\.\d{2})`

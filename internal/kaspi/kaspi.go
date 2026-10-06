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

// Salary is the details Kaspi prints on a top-up that came in as a salary ("salary").
const Salary = "Зарплата"

// Details of top-ups that bring the person's own money in: from a card in another bank,
// by account number, cash put in at a Kaspi ATM or terminal.
var OwnTopUps = []string{"С карты другого банка", "по номеру счета", "В Kaspi Банкомате", "В Kaspi Терминале"}

// Words in the details of a top-up from a lender or a payment to one: microfinance
// organisations, loan payments.
var LoanWords = []string{"MFO", "МФО", "LOAN", "KREDIT", "КРЕДИТ", "ZAIM", "ЗАЙМ"}

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

// HolderPattern finds the account holder on the statement's cover: "АО «Kaspi Bank»
// подтверждает, что <full name>, ИИН …" ("confirms that <full name>, IIN …"). Only the name
// is taken; the IIN (national ID) is never kept.
const HolderPattern = `подтверждает,?\s+что\s+(.+?),?\s+ИИН`

// PeriodPattern matches the statement period line, "за период с 05.10.25 по 05.10.26"
// ("for the period from … to …").
const PeriodPattern = `за период с (\d{2}\.\d{2}\.\d{2}) по (\d{2}\.\d{2}\.\d{2})`

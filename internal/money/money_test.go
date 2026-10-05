package money

import "testing"

func TestParse(t *testing.T) {
	cases := []struct {
		in       string
		minor    int64
		currency string
	}{
		{"4 500,00 ₸", 450000, "KZT"},
		{"4 500,00 ₸", 450000, "KZT"},
		{"4 500 ₸", 450000, "KZT"},
		{"₸4,500.00", 450000, "KZT"},
		{"₸4 500", 450000, "KZT"},
		{"4500", 450000, "KZT"},
		{"1 200", 120000, "KZT"},
		{"990,5 тг", 99050, "KZT"},
		{"$12.50", 1250, "USD"},
		{"12,50 $", 1250, "USD"},
		{"€1.234,56", 123456, "EUR"},
		{"1,234.56 USD", 123456, "USD"},
		{"1.234 €", 123400, "EUR"},
		{"4,500", 450000, "KZT"},
		{"1,234,567.89 KZT", 123456789, "KZT"},
		{"-2 000 ₸", -200000, "KZT"},
		{"−350,00 ₸", -35000, "KZT"},
		{"0,99", 99, "KZT"},
		{"12.5", 1250, "KZT"},
		{"1'250.00 $", 125000, "USD"},
		{"750 ₽", 75000, "RUB"},
		{"+ 10 000,00 ₸", 1000000, "KZT"}, // Kaspi statement format
		{"- 3 280,00 ₸", -328000, "KZT"},
	}
	for _, c := range cases {
		minor, cur, err := Parse(c.in)
		if err != nil {
			t.Errorf("Parse(%q): %v", c.in, err)
			continue
		}
		if minor != c.minor || cur != c.currency {
			t.Errorf("Parse(%q) = %d %s, want %d %s", c.in, minor, cur, c.minor, c.currency)
		}
	}
}

func TestParseErrors(t *testing.T) {
	for _, in := range []string{"", "   ", "₸", "abc", "0,00", "12.345.6", "1,23,456", "4 500,001", "12-3", "1.2.3,45,6"} {
		if m, c, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) = %d %s, want error", in, m, c)
		}
	}
}

func TestFormat(t *testing.T) {
	cases := []struct {
		minor    int64
		currency string
		want     string
	}{
		{450000, "KZT", "4,500 ₸"},
		{1250, "USD", "12.50 $"},
		{99, "KZT", "0.99 ₸"},
		{123456789, "KZT", "1,234,567.89 ₸"},
		{-200000, "KZT", "-2,000 ₸"},
		{100, "CHF", "1 CHF"},
	}
	for _, c := range cases {
		if got := Format(c.minor, c.currency); got != c.want {
			t.Errorf("Format(%d, %s) = %q, want %q", c.minor, c.currency, got, c.want)
		}
	}
}

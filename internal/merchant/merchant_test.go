package merchant

import "testing"

func TestNormalize(t *testing.T) {
	cases := []struct{ in, want string }{
		{"MAGNUM CASH&CARRY", "magnum cash carry"},
		{"Magnum Cash & Carry #12", "magnum cash carry"},
		{"MAGNUM CASH&CARRY 0042 ALMATY KZ", "magnum cash carry"},
		{"ТОО «Small»", "small"},
		{"IP ASANOV", "asanov"},
		{"ИП Асанов А.", "асанов а"},
		{"Yandex.Go", "yandex go"},
		{"SQ *COFFEE BOOM", "coffee boom"},
		{"7-Eleven", "eleven"},
		{"12345", "12345"},
		{"  ", ""},
		{"Starbucks №5", "starbucks"},
	}
	for _, c := range cases {
		if got := Normalize(c.in); got != c.want {
			t.Errorf("Normalize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDisplay(t *testing.T) {
	cases := []struct{ in, want string }{
		{"MAGNUM CASH&CARRY", "Magnum Cash&Carry"},
		{"iHerb", "iHerb"},
		{"  YANDEX   GO ", "Yandex Go"},
		{"ТОО SMALL", "Тоо Small"},
		{"", "unknown merchant"},
		{"MCDONALD'S", "Mcdonald's"},
	}
	for _, c := range cases {
		if got := Display(c.in); got != c.want {
			t.Errorf("Display(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

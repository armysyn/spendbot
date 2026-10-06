package auth

import (
	"strings"
	"testing"
)

func TestHashVerify(t *testing.T) {
	h, err := Hash("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "pbkdf2-sha256$600000$") || strings.Contains(h, "correct horse") {
		t.Fatalf("stored form: %s", h)
	}
	if !Verify("correct horse", h) || Verify("correct hors", h) || Verify("", h) {
		t.Error("verify")
	}
	h2, _ := Hash("correct horse")
	if h == h2 {
		t.Error("the salt must differ")
	}
	for _, bad := range []string{"", "plain", "pbkdf2-sha256$x$a$b", "md5$1$a$b", "pbkdf2-sha256$600000$!!$!!"} {
		if Verify("x", bad) {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestCheckAndTokens(t *testing.T) {
	if Check("short") == nil || Check("пароль12") != nil || Check(strings.Repeat("a", 2000)) == nil {
		t.Error("check")
	}
	tok, hash, err := NewToken()
	if err != nil || len(tok) < 40 || hash != TokenHash(tok) || hash == tok {
		t.Fatalf("token %q %q %v", tok, hash, err)
	}
	if t2, _, _ := NewToken(); t2 == tok {
		t.Error("tokens must be random")
	}
}

// Package auth hashes the page password and makes session tokens. Only the standard library:
// PBKDF2-SHA256 with a random salt for the password, random tokens stored as SHA-256 hashes.
package auth

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Iterations follows the OWASP recommendation for PBKDF2-HMAC-SHA256.
const Iterations = 600_000

// MinLength is the shortest password accepted.
const MinLength = 8

var b64 = base64.RawStdEncoding

// Check reports why a new password is not acceptable; nil — it is.
func Check(password string) error {
	if utf8.RuneCountInString(password) < MinLength {
		return fmt.Errorf("the password needs at least %d characters", MinLength)
	}
	if len(password) > 1024 {
		return errors.New("the password is too long")
	}
	return nil
}

// Hash returns "pbkdf2-sha256$<iterations>$<salt>$<key>" for storing.
func Hash(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, Iterations, 32)
	if err != nil {
		return "", err
	}
	return "pbkdf2-sha256$" + strconv.Itoa(Iterations) + "$" + b64.EncodeToString(salt) + "$" + b64.EncodeToString(key), nil
}

// Verify checks a password against a stored hash in constant time.
func Verify(password, stored string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 || iter > 10_000_000 {
		return false
	}
	salt, err1 := b64.DecodeString(parts[2])
	want, err2 := b64.DecodeString(parts[3])
	if err1 != nil || err2 != nil || len(want) == 0 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

// NewToken returns a random session token for the cookie and its hash for the database:
// a stolen database does not give working cookies.
func NewToken() (token, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, TokenHash(token), nil
}

// TokenHash is the stored form of a session token.
func TokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

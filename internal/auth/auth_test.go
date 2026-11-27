package auth

import (
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

func init() { Cost = bcrypt.MinCost }

var secret = []byte(strings.Repeat("s", 32))

func TestPasswordHashing(t *testing.T) {
	h, err := HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if h == "correct horse" {
		t.Fatal("password stored in plaintext")
	}
	if !CheckPassword(h, "correct horse") {
		t.Error("correct password rejected")
	}
	if CheckPassword(h, "wrong horse") {
		t.Error("wrong password accepted")
	}
	if CheckPassword("", "anything") {
		t.Error("unknown user (empty hash) must never authenticate")
	}
}

func TestPasswordTooLongIsRejected(t *testing.T) {
	// bcrypt ignores bytes past 72; accepting them would mean two different
	// passwords sharing a 72-byte prefix both work.
	if _, err := HashPassword(strings.Repeat("a", MaxPasswordBytes+1)); err == nil {
		t.Fatal("expected error for >72-byte password")
	}
}

func TestTokenRoundTrip(t *testing.T) {
	tk := NewTokens(secret, time.Hour)
	tok, exp, err := tk.Issue(Principal{UserID: 42, Username: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if time.Until(exp) <= 0 {
		t.Fatal("expiry in the past")
	}
	p, err := tk.Parse(tok)
	if err != nil {
		t.Fatal(err)
	}
	if p.UserID != 42 || p.Username != "alice" {
		t.Fatalf("got %+v", p)
	}
}

func TestTokenRejections(t *testing.T) {
	tk := NewTokens(secret, time.Hour)
	valid, _, _ := tk.Issue(Principal{UserID: 1, Username: "bob"})

	expired := NewTokens(secret, time.Hour)
	expired.now = func() time.Time { return time.Now().Add(-2 * time.Hour) }
	expiredTok, _, _ := expired.Issue(Principal{UserID: 1, Username: "bob"})

	otherKey, _, _ := NewTokens([]byte(strings.Repeat("x", 32)), time.Hour).Issue(Principal{UserID: 1, Username: "bob"})

	// "alg: none" attack: unsigned token claiming to be anyone.
	none := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{
		"sub": "1", "username": "bob", "iss": issuer, "exp": time.Now().Add(time.Hour).Unix(),
	})
	noneTok, _ := none.SignedString(jwt.UnsafeAllowNoneSignatureType)

	noExp := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"sub": "1", "username": "bob", "iss": issuer})
	noExpTok, _ := noExp.SignedString(secret)

	wrongIss := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "1", "username": "bob", "iss": "someone-else", "exp": time.Now().Add(time.Hour).Unix(),
	})
	wrongIssTok, _ := wrongIss.SignedString(secret)

	cases := map[string]string{
		"expired":      expiredTok,
		"wrong key":    otherKey,
		"alg none":     noneTok,
		"no expiry":    noExpTok,
		"wrong issuer": wrongIssTok,
		"tampered":     valid[:len(valid)-2] + "xx",
		"garbage":      "not.a.jwt",
		"empty":        "",
	}
	for name, tok := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := tk.Parse(tok); err == nil {
				t.Fatal("expected token to be rejected")
			}
		})
	}
}

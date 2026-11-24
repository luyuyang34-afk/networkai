// Package auth handles password hashing and JWT issuance/verification.
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

// ---------- passwords ----------

// bcrypt only looks at the first 72 bytes; reject longer passwords instead
// of silently truncating them.
const MaxPasswordBytes = 72

// Cost is the bcrypt work factor. Each +1 doubles hashing time; tests lower it.
var Cost = 12

func HashPassword(pw string) (string, error) {
	if len(pw) > MaxPasswordBytes {
		return "", errors.New("password too long")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pw), Cost)
	return string(h), err
}

// dummyHash is compared against when a username does not exist, so a failed
// login takes the same time whether or not the user exists. Without it an
// attacker could enumerate usernames by timing the login endpoint.
// It is generated lazily with the same Cost as real hashes so both paths take
// the same time.
var (
	dummyOnce sync.Once
	dummyHash []byte
)

func CheckPassword(hash, pw string) bool {
	if hash == "" {
		dummyOnce.Do(func() {
			dummyHash, _ = bcrypt.GenerateFromPassword([]byte("timing-equaliser"), Cost)
		})
		bcrypt.CompareHashAndPassword(dummyHash, []byte(pw)) //nolint:errcheck
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// ---------- tokens ----------

const issuer = "networkai"

type Principal struct {
	UserID   int64
	Username string
}

type claims struct {
	Username string `json:"username"`
	jwt.RegisteredClaims
}

type Tokens struct {
	secret []byte
	ttl    time.Duration
	now    func() time.Time
}

func NewTokens(secret []byte, ttl time.Duration) *Tokens {
	return &Tokens{secret: secret, ttl: ttl, now: time.Now}
}

func (t *Tokens) Issue(p Principal) (token string, expiresAt time.Time, err error) {
	now := t.now()
	expiresAt = now.Add(t.ttl)
	c := claims{
		Username: p.Username,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    issuer,
			Subject:   strconv.FormatInt(p.UserID, 10),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
		},
	}
	token, err = jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(t.secret)
	return token, expiresAt, err
}

func (t *Tokens) Parse(token string) (Principal, error) {
	var c claims
	_, err := jwt.ParseWithClaims(token, &c,
		func(*jwt.Token) (any, error) { return t.secret, nil },
		// Pin the algorithm: never let the token header choose it
		// (prevents the classic "alg: none" / algorithm-confusion attacks).
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(issuer),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(t.now),
	)
	if err != nil {
		return Principal{}, err
	}
	id, err := strconv.ParseInt(c.Subject, 10, 64)
	if err != nil || c.Username == "" {
		return Principal{}, fmt.Errorf("malformed token subject")
	}
	return Principal{UserID: id, Username: c.Username}, nil
}

// ---------- request context ----------

type ctxKey struct{}

func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}

// BearerToken extracts the token from an "Authorization: Bearer <t>" header.
func BearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	scheme, tok, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(tok) == "" {
		return "", false
	}
	return strings.TrimSpace(tok), true
}

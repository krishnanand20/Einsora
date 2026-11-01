// Package credential hashes passwords and makes and hashes tokens, for local accounts (ADR-0012).
// Passwords use argon2id; tokens are 256 random bits, stored only as their SHA-256 hash, so a copy
// of the database never yields a usable credential.
package credential

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters: 64 MiB, 3 passes, 2 lanes, above OWASP's minimum. They're stored with each
// hash, so they can be raised later without breaking existing passwords.
const (
	memoryKiB = 64 * 1024
	passes    = 3
	lanes     = 2
	saltLen   = 16
	keyLen    = 32
)

// MaxPasswordBytes bounds any password that's hashed, so a long one can't be a way to make the
// server work hard.
const MaxPasswordBytes = 1024

// params is how the current parameters appear in an encoded hash.
var params = fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$", argon2.Version, memoryKiB, passes, lanes)

// NeedsRehash reports whether encoded was made with other parameters than the current ones. Rehash
// on the next successful sign-in, so every account costs the same to check (and so does the dummy
// hash for unknown emails), and timing can't tell accounts apart.
func NeedsRehash(encoded string) bool { return !strings.HasPrefix(encoded, params) }

// HashPassword returns an argon2id hash in the standard encoded form:
// $argon2id$v=19$m=65536,t=3,p=2$<salt>$<key>.
func HashPassword(password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("read random salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, passes, memoryKiB, lanes, keyLen)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, memoryKiB, passes, lanes, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// VerifyPassword reports whether password matches encoded, in constant time. A malformed hash
// never matches.
func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != fmt.Sprintf("v=%d", argon2.Version) {
		return false
	}
	var memory, time uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &threads); err != nil ||
		memory == 0 || memory > 1<<20 || time == 0 || time > 10 || threads == 0 {
		return false // also bounds the work a tampered hash could demand
	}
	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := b64.DecodeString(parts[5])
	if err != nil || len(want) == 0 || len(want) > 64 {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, time, memory, threads, uint32(len(want))) //nolint:gosec // len(want) ≤ 64
	return subtle.ConstantTimeCompare(got, want) == 1
}

// NewToken returns a random token with a recognizable prefix ("tsk_" for API tokens), and the
// hash to store. The token itself is shown once and never stored.
func NewToken(prefix string) (token, hash string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("read random token: %w", err)
	}
	token = prefix + base64.RawURLEncoding.EncodeToString(raw)
	return token, HashToken(token), nil
}

// HashToken is the stored form of a token: its SHA-256, in hex. A token has 256 random bits, so a
// fast hash is enough; it isn't a password.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// ErrWeakPassword explains why a new password was refused; its message is safe to show.
var ErrWeakPassword = errors.New("a password needs at least 12 characters (at most 1024 bytes), " +
	"must differ from the email, and can't be a common password or mostly one character")

// common reports the passwords of 12 or more characters that guessing tools try first, compared
// without case, spaces, or punctuation. Built once, and never changed after.
var common = sync.OnceValue(func() map[string]bool {
	set := map[string]bool{}
	for _, p := range strings.Fields(`
		password1234 password12345 password123456 password1234567 password12345678 passwordpassword
		123456789012 1234567890123 12345678901234 123456789123 123123123123 111111111111 000000000000
		qwertyuiop12 qwertyuiop123 qwerty123456 qwerty1234567 qwertyuiopasdf qwertyuiopasdfgh
		1q2w3e4r5t6y 1qaz2wsx3edc 1qaz2wsx3edc4rfv zaq12wsxcde3 asdfghjkl123 asdfghjklqwe
		iloveyou1234 iloveyouiloveyou letmein12345 welcome12345 welcome123456 changeme1234
		changeme12345 administrator administrator1 admin1234567 adminadmin123 rootroot1234
		trustno11234 sunshine1234 football1234 baseball1234 superman1234 princess1234 dragon123456
		monkey123456 master123456 abc123456789 abcdefghijkl abcdefghijklmn abcd12345678 abcd1234abcd
		passw0rd1234 p4ssw0rd1234 letmeinletmein whatever1234 starwars1234 pokemon12345
		einsora12345 einsora123456 einsoraeinsora
	`) {
		set[p] = true
	}
	return set
})

// fold reduces a password to what guessing tools vary least: lowercase letters and digits.
func fold(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'A' && r <= 'Z':
			return r + 'a' - 'A'
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		}
		return -1
	}, s)
}

// CheckNewPassword refuses passwords that are too short or too common to resist guessing, or long
// enough to be a way to make the server work hard.
func CheckNewPassword(password, email string) error {
	distinct := map[rune]bool{}
	for _, r := range password {
		distinct[r] = true
	}
	if len([]rune(password)) < 12 || len(password) > MaxPasswordBytes || strings.EqualFold(password, email) ||
		len(distinct) < 5 || common()[fold(password)] {
		return ErrWeakPassword
	}
	return nil
}

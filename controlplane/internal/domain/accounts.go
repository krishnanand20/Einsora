package domain

import (
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

// User is a local account (ADR-0012): someone who signs in with an email and a password. Each
// belongs to one tenant for now; invites and teams arrive in Phase 2.
type User struct {
	ID           uuid.UUID
	TenantID     uuid.UUID
	Email        string // normalized: trimmed and lowercase
	PasswordHash string // argon2id, from package credential; never the password
	CreatedAt    time.Time
	DisabledAt   time.Time // zero while the account is active
}

// Session is a signed-in browser. Only the hash of its token is stored.
type Session struct {
	TokenHash string
	UserID    uuid.UUID
	CreatedAt time.Time
	ExpiresAt time.Time
}

// APIToken is a credential for the CLI and scripts. Only the hash of the token is stored; the token
// itself is shown once, when it's created.
type APIToken struct {
	ID         uuid.UUID
	UserID     uuid.UUID
	Name       string
	Prefix     string // the first characters, so its owner can recognize it in a list
	TokenHash  string
	CreatedAt  time.Time
	ExpiresAt  time.Time // zero for a token that doesn't expire
	LastUsedAt time.Time // zero until first used
	RevokedAt  time.Time // zero while it's valid
}

// Usable reports whether the token may still authenticate at time now.
func (t APIToken) Usable(now time.Time) bool {
	return t.RevokedAt.IsZero() && (t.ExpiresAt.IsZero() || now.Before(t.ExpiresAt))
}

// AccessTicket lets one person open one workload once, through the access proxy (ADR-0017). Only
// its hash is stored.
type AccessTicket struct {
	Hash       string
	TenantID   uuid.UUID
	WorkloadID uuid.UUID
	Subject    string // who opened it, for the operator's log
	ExpiresAt  time.Time
	UsedAt     time.Time // zero until used
}

// NormalizeEmail trims and lowercases an email, and reports whether it looks like one: a local part,
// an @, and a domain, with no spaces or control characters, at most 254 bytes. Sign-in compares the
// normalized form.
func NormalizeEmail(email string) (string, bool) {
	email = strings.ToLower(strings.TrimSpace(email))
	local, domain, ok := strings.Cut(email, "@")
	if !ok || local == "" || !strings.Contains(domain, ".") || strings.Contains(domain, "@") || len(email) > 254 ||
		strings.IndexFunc(email, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return "", false
	}
	return email, true
}

// ValidTokenName is a short, printable label for an API token.
func ValidTokenName(name string) bool {
	return name != "" && len(name) <= 64 && strings.IndexFunc(name, func(r rune) bool { return !unicode.IsPrint(r) }) < 0
}

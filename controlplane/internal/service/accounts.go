package service

import (
	"context"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/krishnanand20/tensorafi/controlplane/internal/credential"
	"github.com/krishnanand20/tensorafi/controlplane/internal/domain"
	"github.com/krishnanand20/tensorafi/controlplane/internal/store"
)

// Local accounts (ADR-0012): one owner account today; invites and teams arrive in Phase 2.

const (
	// SessionTTL is how long a browser stays signed in.
	SessionTTL = 7 * 24 * time.Hour
	// TokenPrefix marks API tokens, so they're recognizable (and findable by secret scanners).
	TokenPrefix = "tsk_"
	// maxTokenTTL bounds an API token's lifetime; zero means it doesn't expire.
	maxTokenTTL = 366 * 24 * time.Hour
	// ownerQuota is the owner's quota on every class at setup: it's their machine.
	ownerQuota = 64
	// touchEvery limits how often a token's last-used time is written, so API calls stay reads.
	touchEvery = time.Minute
)

// SetupInput creates the first account: the machine's owner.
type SetupInput struct {
	Email      string
	Password   string
	TenantSlug string // the owner's tenant; created as trusted if it doesn't exist
	TenantName string
}

// SetupOwner creates the first account, its trusted tenant, and a quota on every class. It refuses
// once any account exists, so it can't be used to take over an installation. It's called by
// `tensorafi setup`, which runs on the machine itself with access to the store.
func (s *Service) SetupOwner(ctx context.Context, in SetupInput) (domain.User, error) {
	email, ok := domain.NormalizeEmail(in.Email)
	if !ok {
		return domain.User{}, domain.Errorf(domain.CodeInvalid, "that doesn't look like an email address")
	}
	if err := credential.CheckNewPassword(in.Password, email); err != nil {
		return domain.User{}, domain.Errorf(domain.CodeInvalid, "%v", err)
	}
	if !domain.ValidSlug(in.TenantSlug) {
		return domain.User{}, domain.Errorf(domain.CodeInvalid, "tenant must be 1-40 lowercase letters, digits, or '-'")
	}
	var hash string
	var hashErr error
	if err := s.withHashSlot(ctx, func() { hash, hashErr = credential.HashPassword(in.Password) }); err != nil {
		return domain.User{}, err // slow on purpose, so outside any transaction
	}
	if hashErr != nil {
		return domain.User{}, hashErr
	}
	now := s.now().UTC()
	u := domain.User{ID: uuid.Must(uuid.NewV7()), Email: email, PasswordHash: hash, CreatedAt: now}
	err := s.store.InTx(ctx, func(q store.Queries) error {
		if err := q.LockUsers(ctx); err != nil {
			return err
		}
		if n, err := q.CountUsers(ctx); err != nil || n > 0 {
			if err != nil {
				return err
			}
			return domain.Errorf(domain.CodeConflict, "an account already exists; setup only creates the first one")
		}
		t, err := q.TenantBySlug(ctx, in.TenantSlug)
		switch {
		case domain.IsNotFound(err):
			name := in.TenantName
			if name == "" {
				name = in.TenantSlug
			}
			t = domain.Tenant{ID: uuid.Must(uuid.NewV7()), Slug: in.TenantSlug, Name: name, Tier: domain.TierTrusted, CreatedAt: now}
			if err := q.CreateTenant(ctx, t); err != nil {
				return err
			}
			for _, g := range s.catalog.GPUClasses {
				if err := q.SetQuota(ctx, domain.Quota{TenantID: t.ID, GPUClass: g.ID, MaxGPUs: ownerQuota}); err != nil {
					return err
				}
			}
		case err != nil:
			return err
		case t.Tier != domain.TierTrusted:
			return domain.Errorf(domain.CodeConflict, "tenant %q exists and isn't trusted, so it can't hold the owner", in.TenantSlug)
		}
		u.TenantID = t.ID
		return q.CreateUser(ctx, u)
	})
	return u, err
}

// Sign-in limits (per 15 minutes). The strict one is per account and caller pair, so nobody can
// lock the owner out just by knowing their email. The looser one per caller (an IPv4 address or an
// IPv6 /64) bounds guessing across accounts and lets each caller check only a couple of passwords
// at a time. A third, looser still, applies to an IPv6 /48, which one person can easily hold whole,
// without letting one subscriber on a shared /48 block the rest. hashSlots caps how fast passwords
// are checked at all, and CheckNewPassword refuses short and common passwords. That keeps online
// guessing slow, but someone with many addresses can still guess slowly: passwords must not be
// guessable.
const (
	maxPairFailures    = 10
	maxClientFailures  = 30
	maxClientInFlight  = 2
	maxNetworkFailures = 100
	maxNetworkInFlight = 4
)

// limiterKeys turns a caller's address into its key (an IPv4 address or an IPv6 /64) and, for
// IPv6, its network's (the /48; empty for IPv4).
func limiterKeys(client string) (caller, network string) {
	a, err := netip.ParseAddr(client)
	if err != nil || !a.Is6() {
		return client, ""
	}
	p64, _ := a.Prefix(64)
	p48, _ := a.Prefix(48)
	return p64.String(), p48.String()
}

// beginAttempt reserves a password check for account (an email, or a user ID) from client, under
// the pair, caller, and network limits. The returned func ends it.
func (s *Service) beginAttempt(now time.Time, account, client string) (func(failed bool), error) {
	caller, network := limiterKeys(client)
	limits := []limit{
		{key: "pair:" + account + "|" + caller, max: maxPairFailures},
		{key: "client:" + caller, max: maxClientFailures, maxInFlight: maxClientInFlight},
	}
	if network != "" {
		limits = append(limits, limit{key: "network:" + network, max: maxNetworkFailures, maxInFlight: maxNetworkInFlight})
	}
	keys := make([]string, len(limits))
	for i, l := range limits {
		keys[i] = l.key
	}
	wait, ok := s.logins.begin(now, limits...)
	if !ok {
		return nil, domain.Errorf(domain.CodeTooManyRequests, "too many attempts; try again in %d minute(s)", int(wait.Minutes())+1)
	}
	return func(failed bool) { s.logins.end(s.now().UTC(), failed, keys...) }, nil
}

// errWrongPassword is the same for an unknown email and a wrong password, so sign-in doesn't
// reveal which accounts exist.
var errWrongPassword = domain.Errorf(domain.CodeUnauthenticated, "wrong email or password")

// withHashSlot runs fn, which hashes a password, once a hashing slot is free; it gives up after a
// few seconds rather than queueing without bound.
func (s *Service) withHashSlot(ctx context.Context, fn func()) error {
	wait := time.NewTimer(3 * time.Second)
	defer wait.Stop()
	select {
	case s.hashSlots <- struct{}{}:
		defer func() { <-s.hashSlots }()
		fn()
		return nil
	case <-wait.C:
		return domain.Errorf(domain.CodeTooManyRequests, "the server is busy checking other passwords; try again in a moment")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Login checks an email and password and starts a session, returning the session token to put in
// a cookie. client identifies the caller (its address) for slowing down guessing.
func (s *Service) Login(ctx context.Context, email, password, client string) (string, error) {
	dummy := s.dummy.get() // made before anything else, so the first sign-in costs the same for every email
	if len(password) > credential.MaxPasswordBytes {
		return "", errWrongPassword // it can't match, so it isn't checked or counted
	}
	now := s.now().UTC()
	email, emailOK := domain.NormalizeEmail(email)
	end, err := s.beginAttempt(now, email, client)
	if err != nil {
		return "", err
	}
	failed := true // until the password checks out; counted when the attempt ends
	defer func() { end(failed) }()

	var u domain.User
	found := false
	if emailOK {
		err := s.store.InTx(ctx, func(q store.Queries) error {
			var err error
			u, err = q.UserByEmail(ctx, email)
			return err
		})
		switch {
		case err == nil:
			found = true
		case !domain.IsNotFound(err):
			failed = false
			return "", err
		}
	}
	// Unknown emails are checked against the dummy hash, at the same cost as real ones. Hashing
	// happens outside any transaction: it's slow on purpose.
	hash := dummy
	if found {
		hash = u.PasswordHash
	}
	matched := false
	if err := s.withHashSlot(ctx, func() { matched = credential.VerifyPassword(hash, password) }); err != nil {
		failed = false // busy, not a wrong guess
		return "", err
	}
	if !matched || !found {
		return "", errWrongPassword
	}
	failed = false
	if !u.DisabledAt.IsZero() {
		return "", domain.Errorf(domain.CodeForbidden, "this account is disabled")
	}
	caller, _ := limiterKeys(client)
	s.logins.reset("pair:" + email + "|" + caller) // a success clears this pair's failures only

	// Hashes made with older parameters are upgraded, so every account costs the same to check.
	rehashed := ""
	if credential.NeedsRehash(u.PasswordHash) {
		_ = s.withHashSlot(ctx, func() { rehashed, _ = credential.HashPassword(password) }) // best effort
	}
	token, tokenHash, err := credential.NewToken("")
	if err != nil {
		return "", err
	}
	err = s.store.InTx(ctx, func(q store.Queries) error {
		// The password was checked outside this transaction. If it changed meanwhile, or the
		// account was disabled, the old password no longer opens a session. The lock makes a
		// password change wait for this session, so its "sign out everywhere" includes it.
		if err := q.LockUser(ctx, u.ID); err != nil {
			return err
		}
		current, err := q.UserByID(ctx, u.ID)
		if err != nil {
			return err
		}
		if current.PasswordHash != u.PasswordHash || !current.DisabledAt.IsZero() {
			return errWrongPassword
		}
		if rehashed != "" {
			if err := q.SetPasswordHash(ctx, u.ID, rehashed); err != nil {
				return err
			}
		}
		if err := q.DeleteExpiredSessions(ctx, now); err != nil {
			return err
		}
		return q.CreateSession(ctx, domain.Session{TokenHash: tokenHash, UserID: u.ID, CreatedAt: now, ExpiresAt: now.Add(SessionTTL)})
	})
	return token, err
}

// Logout ends the session with token. Ending one that's already gone is fine.
func (s *Service) Logout(ctx context.Context, token string) error {
	return s.store.InTx(ctx, func(q store.Queries) error { return q.DeleteSession(ctx, credential.HashToken(token)) })
}

var errSignIn = domain.Errorf(domain.CodeUnauthenticated, "sign in again: the session has ended")

// PrincipalForSession resolves a browser's session token.
func (s *Service) PrincipalForSession(ctx context.Context, token string) (domain.Principal, error) {
	now := s.now().UTC()
	var p domain.Principal
	err := s.store.InTx(ctx, func(q store.Queries) error {
		sess, err := q.SessionByTokenHash(ctx, credential.HashToken(token))
		if domain.IsNotFound(err) {
			return errSignIn
		}
		if err != nil {
			return err
		}
		if !now.Before(sess.ExpiresAt) {
			return errSignIn
		}
		p, err = principalFor(ctx, q, sess.UserID)
		p.Via, p.Credential = domain.AuthSession, "browser session"
		return err
	})
	return p, err
}

// PrincipalForToken resolves an API token (TokenPrefix…).
func (s *Service) PrincipalForToken(ctx context.Context, token string) (domain.Principal, error) {
	invalid := domain.Errorf(domain.CodeUnauthenticated, "invalid, expired, or revoked API token")
	if len(token) < len(TokenPrefix)+16 || token[:len(TokenPrefix)] != TokenPrefix {
		return domain.Principal{}, invalid
	}
	now := s.now().UTC()
	var p domain.Principal
	err := s.store.InTx(ctx, func(q store.Queries) error {
		t, err := q.APITokenByHash(ctx, credential.HashToken(token))
		if domain.IsNotFound(err) {
			return invalid
		}
		if err != nil {
			return err
		}
		if !t.Usable(now) {
			return invalid
		}
		if now.Sub(t.LastUsedAt) >= touchEvery {
			if err := q.TouchAPIToken(ctx, t.ID, now); err != nil {
				return err
			}
		}
		p, err = principalFor(ctx, q, t.UserID)
		p.Via, p.Credential = domain.AuthToken, fmt.Sprintf("API token %q (%s…)", t.Name, t.Prefix)
		return err
	})
	return p, err
}

// principalFor builds the identity of an active user; a disabled one can't authenticate.
func principalFor(ctx context.Context, q store.Queries, userID uuid.UUID) (domain.Principal, error) {
	u, err := q.UserByID(ctx, userID)
	if err != nil {
		return domain.Principal{}, err
	}
	if !u.DisabledAt.IsZero() {
		return domain.Principal{}, domain.Errorf(domain.CodeUnauthenticated, "this account is disabled")
	}
	t, err := q.TenantByID(ctx, u.TenantID)
	if err != nil {
		return domain.Principal{}, err
	}
	return domain.Principal{Subject: u.Email, Email: u.Email, TenantSlug: t.Slug, UserID: u.ID}, nil
}

func requireAccount(p domain.Principal) error {
	if p.UserID == uuid.Nil {
		return domain.Errorf(domain.CodeInvalid, "this needs a local account (TENSORAFI_AUTH_MODE=local)")
	}
	return nil
}

// requireSession refuses an API token for things that would let a stolen token outlive its
// revocation: changing the password, and making more tokens. Those need a signed-in browser.
func requireSession(p domain.Principal) error {
	if err := requireAccount(p); err != nil {
		return err
	}
	if p.Via != domain.AuthSession {
		return domain.Errorf(domain.CodeForbidden, "this needs a signed-in browser, not an API token")
	}
	return nil
}

// checkPassword verifies the signed-in user's password before something a stolen session alone
// mustn't do, slowing down guessing as sign-in does.
func (s *Service) checkPassword(ctx context.Context, p domain.Principal, password, client string) (domain.User, error) {
	if err := requireSession(p); err != nil {
		return domain.User{}, err
	}
	wrong := domain.Errorf(domain.CodeForbidden, "your current password is wrong")
	if len(password) > credential.MaxPasswordBytes {
		return domain.User{}, wrong
	}
	// Per user and caller, like sign-in: someone holding a stolen session can't use up the owner's
	// attempts from another address.
	end, err := s.beginAttempt(s.now().UTC(), "user:"+p.UserID.String(), client)
	if err != nil {
		return domain.User{}, err
	}
	failed := true
	defer func() { end(failed) }()
	var u domain.User
	if err := s.store.InTx(ctx, func(q store.Queries) error {
		var err error
		u, err = q.UserByID(ctx, p.UserID)
		return err
	}); err != nil {
		failed = false
		return domain.User{}, err
	}
	matched := false
	if err := s.withHashSlot(ctx, func() { matched = credential.VerifyPassword(u.PasswordHash, password) }); err != nil {
		failed = false
		return domain.User{}, err
	}
	if !matched {
		return domain.User{}, wrong
	}
	failed = false
	return u, nil
}

// ChangePassword checks the current password, sets a new one, and signs the user out of every
// other session. currentSession is the caller's session token, which stays signed in. It returns
// how many API tokens still work, so the user can revoke them too if the password leaked.
func (s *Service) ChangePassword(ctx context.Context, p domain.Principal, current, next, currentSession, client string) (int, error) {
	u, err := s.checkPassword(ctx, p, current, client)
	if err != nil {
		return 0, err
	}
	if err := credential.CheckNewPassword(next, u.Email); err != nil {
		return 0, domain.Errorf(domain.CodeInvalid, "%v", err)
	}
	var hash string
	var hashErr error
	if err := s.withHashSlot(ctx, func() { hash, hashErr = credential.HashPassword(next) }); err != nil {
		return 0, err
	}
	if hashErr != nil {
		return 0, hashErr
	}
	now := s.now().UTC()
	live := 0
	err = s.store.InTx(ctx, func(q store.Queries) error {
		if err := q.LockUser(ctx, u.ID); err != nil {
			return err
		}
		latest, err := q.UserByID(ctx, u.ID)
		if err != nil {
			return err
		}
		if latest.PasswordHash != u.PasswordHash {
			return domain.Errorf(domain.CodeConflict, "the password changed meanwhile; sign in again")
		}
		if err := q.SetPasswordHash(ctx, u.ID, hash); err != nil {
			return err
		}
		if err := q.DeleteOtherSessions(ctx, u.ID, credential.HashToken(currentSession)); err != nil {
			return err
		}
		tokens, err := q.ListAPITokens(ctx, u.ID)
		for _, t := range tokens {
			if t.Usable(now) {
				live++
			}
		}
		return err
	})
	return live, err
}

// ResetPassword sets a new password for an existing account and signs it out everywhere. It's
// for `tensorafi setup -reset-password`, which needs access to the store: the way back in for an
// owner who forgot their password, since there's no email reset.
func (s *Service) ResetPassword(ctx context.Context, email, password string) (domain.User, error) {
	email, ok := domain.NormalizeEmail(email)
	if !ok {
		return domain.User{}, domain.Errorf(domain.CodeInvalid, "that doesn't look like an email address")
	}
	if err := credential.CheckNewPassword(password, email); err != nil {
		return domain.User{}, domain.Errorf(domain.CodeInvalid, "%v", err)
	}
	var hash string
	var hashErr error
	if err := s.withHashSlot(ctx, func() { hash, hashErr = credential.HashPassword(password) }); err != nil {
		return domain.User{}, err
	}
	if hashErr != nil {
		return domain.User{}, hashErr
	}
	var u domain.User
	err := s.store.InTx(ctx, func(q store.Queries) error {
		var err error
		u, err = q.UserByEmail(ctx, email)
		if domain.IsNotFound(err) {
			return domain.Errorf(domain.CodeNotFound, "no account has the email %s", email)
		}
		if err != nil {
			return err
		}
		if err := q.LockUser(ctx, u.ID); err != nil { // waits for a sign-in in progress, then signs it out too
			return err
		}
		if err := q.SetPasswordHash(ctx, u.ID, hash); err != nil {
			return err
		}
		return q.DeleteOtherSessions(ctx, u.ID, "")
	})
	return u, err
}

// CreateAPIToken makes a token for the caller. The token is returned once and never stored; ttl
// zero means it doesn't expire. A token outlives the session that makes it, and survives a password
// change, so making one needs the password again: a stolen session, or a script running on the
// dashboard's origin, can't make one by itself.
func (s *Service) CreateAPIToken(ctx context.Context, p domain.Principal, name string, ttl time.Duration, password, client string) (string, domain.APIToken, error) {
	if err := requireSession(p); err != nil {
		return "", domain.APIToken{}, err
	}
	if !domain.ValidTokenName(name) {
		return "", domain.APIToken{}, domain.Errorf(domain.CodeInvalid, "a token name is 1-64 printable characters")
	}
	if ttl < 0 || ttl > maxTokenTTL {
		return "", domain.APIToken{}, domain.Errorf(domain.CodeInvalid, "a token can last at most 366 days (or not expire)")
	}
	if _, err := s.checkPassword(ctx, p, password, client); err != nil {
		return "", domain.APIToken{}, err
	}
	token, hash, err := credential.NewToken(TokenPrefix)
	if err != nil {
		return "", domain.APIToken{}, err
	}
	now := s.now().UTC()
	t := domain.APIToken{ID: uuid.Must(uuid.NewV7()), UserID: p.UserID, Name: name, Prefix: token[:len(TokenPrefix)+4],
		TokenHash: hash, CreatedAt: now}
	if ttl > 0 {
		t.ExpiresAt = now.Add(ttl)
	}
	err = s.store.InTx(ctx, func(q store.Queries) error { return q.CreateAPIToken(ctx, t) })
	if code, _ := domain.CodeOf(err); code == domain.CodeConflict {
		return "", domain.APIToken{}, domain.Errorf(domain.CodeConflict, "you already have a token named %q", name)
	}
	return token, t, err
}

// ListAPITokens lists the caller's own tokens, including revoked and expired ones.
func (s *Service) ListAPITokens(ctx context.Context, p domain.Principal) ([]domain.APIToken, error) {
	if err := requireAccount(p); err != nil {
		return nil, err
	}
	var out []domain.APIToken
	err := s.store.InTx(ctx, func(q store.Queries) error {
		var err error
		out, err = q.ListAPITokens(ctx, p.UserID)
		return err
	})
	return out, err
}

// RevokeAPIToken revokes one of the caller's tokens. Another user's token is not found.
func (s *Service) RevokeAPIToken(ctx context.Context, p domain.Principal, id uuid.UUID) error {
	if err := requireAccount(p); err != nil {
		return err
	}
	return s.store.InTx(ctx, func(q store.Queries) error { return q.RevokeAPIToken(ctx, p.UserID, id, s.now().UTC()) })
}

// dummyHash is a real argon2id hash of nothing anyone knows, made once on first use, for checking
// unknown emails at the same cost as real ones.
type dummyHash struct {
	once sync.Once
	hash string
}

func (d *dummyHash) get() string {
	d.once.Do(func() {
		token, _, err := credential.NewToken("")
		if err != nil {
			token = "unguessable"
		}
		d.hash, _ = credential.HashPassword(token) // on error, the empty hash never matches
	})
	return d.hash
}

// loginLimiter slows down password guessing. An attempt is counted as it starts, so attempts made
// in parallel can't slip past the limit while each is still being checked. Only attempts whose
// password was actually checked are recorded as failures, so how fast failures can be recorded,
// and how much memory they take, is bounded by hashSlots; the map is capped as well.
type loginLimiter struct {
	mu        sync.Mutex
	failures  map[string][]time.Time // newest last
	inFlight  map[string]int
	lastSweep time.Time
}

type limit struct {
	key         string
	max         int // failures in the window plus attempts in progress
	maxInFlight int // attempts in progress at once; 0 means only max applies
}

const (
	loginWindow     = 15 * time.Minute
	maxLimiterKeys  = 20000
	maxFailuresKept = 2 * maxNetworkFailures // more than any limit, so the count stays exact
	sweepEvery      = time.Minute
)

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{failures: map[string][]time.Time{}, inFlight: map[string]int{}}
}

func (l *loginLimiter) recent(key string, now time.Time) []time.Time {
	var kept []time.Time
	for _, t := range l.failures[key] {
		if now.Sub(t) < loginWindow {
			kept = append(kept, t)
		}
	}
	return kept
}

// begin reserves an attempt under every limit, or reports how long to wait when recent failures
// plus attempts in progress have reached one.
func (l *loginLimiter) begin(now time.Time, limits ...limit) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, lim := range limits {
		inFlight := l.inFlight[lim.key]
		if lim.maxInFlight > 0 && inFlight >= lim.maxInFlight {
			return time.Second, false
		}
		r := l.recent(lim.key, now)
		if len(r)+inFlight >= lim.max {
			if len(r) == 0 {
				return time.Second, false // only attempts in progress: try again shortly
			}
			// Wait until enough failures leave the window.
			return loginWindow - now.Sub(r[max(0, len(r)+inFlight-lim.max)]), false
		}
	}
	for _, lim := range limits {
		l.inFlight[lim.key]++
	}
	return 0, true
}

// end finishes an attempt begun with the same keys, recording a failure if it failed.
func (l *loginLimiter) end(now time.Time, failed bool, keys ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range keys {
		if l.inFlight[k]--; l.inFlight[k] <= 0 {
			delete(l.inFlight, k)
		}
		if failed {
			f := append(l.recent(k, now), now)
			l.failures[k] = f[max(0, len(f)-maxFailuresKept):]
		}
	}
	if now.Sub(l.lastSweep) >= sweepEvery || len(l.failures) > maxLimiterKeys {
		l.sweep(now)
	}
}

// sweep forgets keys with no recent failures. If too many are left, it forgets the least recently
// failed, pair keys first: the per-caller keys still bound guessing.
func (l *loginLimiter) sweep(now time.Time) {
	l.lastSweep = now
	for k, f := range l.failures {
		if len(f) == 0 || now.Sub(f[len(f)-1]) >= loginWindow {
			delete(l.failures, k)
		}
	}
	if len(l.failures) <= maxLimiterKeys {
		return
	}
	keys := slices.Collect(maps.Keys(l.failures))
	slices.SortFunc(keys, func(a, b string) int {
		ac, bc := strings.HasPrefix(a, "client:"), strings.HasPrefix(b, "client:")
		if ac != bc {
			if ac {
				return 1
			}
			return -1
		}
		fa, fb := l.failures[a], l.failures[b]
		return fa[len(fa)-1].Compare(fb[len(fb)-1])
	})
	for _, k := range keys[:len(keys)-maxLimiterKeys*9/10] {
		delete(l.failures, k)
	}
}

func (l *loginLimiter) reset(keys ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range keys {
		delete(l.failures, k)
	}
}

// String keeps the password out of logs if a SetupInput is ever printed by mistake.
func (SetupInput) String() string { return "SetupInput{redacted}" }

package service_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/argon2"

	"github.com/krishnanand20/einsora/controlplane/internal/credential"
	"github.com/krishnanand20/einsora/controlplane/internal/domain"
	"github.com/krishnanand20/einsora/controlplane/internal/service"
	"github.com/krishnanand20/einsora/controlplane/internal/store"
	"github.com/krishnanand20/einsora/controlplane/internal/store/memstore"
	"github.com/krishnanand20/einsora/controlplane/internal/testutil"
)

const ownerPassword = "correct horse battery staple"

// accounts returns a service on an empty store with the owner set up, and its clock.
func accounts(t *testing.T) (*service.Service, store.Store, *testutil.Clock, domain.User) {
	t.Helper()
	st := memstore.New()
	clock := testutil.NewClock(testutil.T0)
	svc := service.New(st, testutil.Catalog(t), clock.Now)
	owner, err := svc.SetupOwner(context.Background(), service.SetupInput{Email: " Owner@Example.com ", Password: ownerPassword, TenantSlug: "home"})
	if err != nil {
		t.Fatal(err)
	}
	return svc, st, clock, owner
}

func TestSetupOwnerRunsOnce(t *testing.T) {
	svc, st, _, owner := accounts(t)
	ctx := context.Background()
	if owner.Email != "owner@example.com" {
		t.Errorf("email = %q, want it normalized", owner.Email)
	}
	err := st.InTx(ctx, func(q store.Queries) error {
		tn, err := q.TenantByID(ctx, owner.TenantID)
		if err != nil || tn.Tier != domain.TierTrusted || tn.Slug != "home" {
			t.Errorf("owner's tenant = %+v, %v; want trusted home", tn, err)
		}
		if limit, _ := q.LockQuota(ctx, owner.TenantID, "l40s-48gb"); limit == 0 {
			t.Error("the owner needs a quota on every class")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.SetupOwner(ctx, service.SetupInput{Email: "intruder@example.com", Password: ownerPassword, TenantSlug: "home"})
	wantCode(t, err, domain.CodeConflict)
}

func TestSetupRefusesWeakInput(t *testing.T) {
	svc := service.New(memstore.New(), testutil.Catalog(t), nil)
	for name, in := range map[string]service.SetupInput{
		"bad email":      {Email: "nope", Password: ownerPassword, TenantSlug: "home"},
		"short password": {Email: "a@b.co", Password: "short", TenantSlug: "home"},
		"bad tenant":     {Email: "a@b.co", Password: ownerPassword, TenantSlug: "Home Lab"},
	} {
		if _, err := svc.SetupOwner(context.Background(), in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestLoginAndSessions(t *testing.T) {
	svc, st, clock, owner := accounts(t)
	ctx := context.Background()

	if _, err := svc.Login(ctx, "owner@example.com", "wrong password!!", "10.0.0.1"); err == nil {
		t.Fatal("a wrong password signed in")
	}
	_, unknownErr := svc.Login(ctx, "nobody@example.com", ownerPassword, "10.0.0.1")
	_, wrongErr := svc.Login(ctx, "owner@example.com", "wrong password!!", "10.0.0.1")
	if unknownErr == nil || wrongErr == nil || unknownErr.Error() != wrongErr.Error() {
		t.Errorf("unknown email %v vs wrong password %v: must look the same", unknownErr, wrongErr)
	}

	token, err := svc.Login(ctx, "OWNER@example.com", ownerPassword, "10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	p, err := svc.PrincipalForSession(ctx, token)
	if err != nil || p.UserID != owner.ID || p.TenantSlug != "home" || p.Subject != "owner@example.com" {
		t.Fatalf("session principal = %+v, %v", p, err)
	}
	_ = st.InTx(ctx, func(q store.Queries) error {
		if _, err := q.SessionByTokenHash(ctx, token); !domain.IsNotFound(err) {
			t.Error("the session token itself must never be stored, only its hash")
		}
		return nil
	})

	clock.Advance(service.SessionTTL)
	_, err = svc.PrincipalForSession(ctx, token)
	wantCode(t, err, domain.CodeUnauthenticated)
}

func TestLoginSlowsDownGuessing(t *testing.T) {
	svc, _, clock, _ := accounts(t)
	ctx := context.Background()
	for range 10 {
		_, _ = svc.Login(ctx, "owner@example.com", "wrong password!!", "10.0.0.1")
	}
	_, err := svc.Login(ctx, "owner@example.com", ownerPassword, "10.0.0.1")
	wantCode(t, err, domain.CodeTooManyRequests)
	// Knowing the owner's email isn't enough to lock them out: from elsewhere, they still get in.
	if _, err := svc.Login(ctx, "owner@example.com", ownerPassword, "10.0.0.2"); err != nil {
		t.Fatalf("the owner, from another address: %v", err)
	}
	clock.Advance(16 * time.Minute)
	if _, err := svc.Login(ctx, "owner@example.com", ownerPassword, "10.0.0.1"); err != nil {
		t.Fatalf("after the window: %v", err)
	}

	// One caller guessing across many emails is limited too.
	for i := range 30 {
		_, _ = svc.Login(ctx, fmt.Sprintf("guess%d@example.com", i), "wrong password!!", "2001:db8:7:1::1")
	}
	_, err = svc.Login(ctx, "owner@example.com", ownerPassword, "2001:db8:7:1::2") // the same /64
	wantCode(t, err, domain.CodeTooManyRequests)
	// A neighbour on the same IPv6 /48 isn't blocked by one subscriber's guessing.
	if _, err := svc.Login(ctx, "owner@example.com", ownerPassword, "2001:db8:7:2::1"); err != nil {
		t.Errorf("another /64 in the /48: %v", err)
	}
}

// Someone holding a stolen session can't use up the owner's password checks from elsewhere: the
// limit is per user and caller.
func TestPasswordChecksAreLimitedPerCaller(t *testing.T) {
	svc, _, _, _ := accounts(t)
	ctx := context.Background()
	session, _ := svc.Login(ctx, "owner@example.com", ownerPassword, "10.0.0.1")
	p, _ := svc.PrincipalForSession(ctx, session)
	for range 10 {
		_, _, _ = svc.CreateAPIToken(ctx, p, "x", 0, "wrong password!!", "203.0.113.66")
	}
	_, _, err := svc.CreateAPIToken(ctx, p, "x", 0, ownerPassword, "203.0.113.66")
	wantCode(t, err, domain.CodeTooManyRequests)
	if _, err := svc.ChangePassword(ctx, p, ownerPassword, "a brand new passphrase", session, "10.0.0.1"); err != nil {
		t.Errorf("the owner, from their own address: %v", err)
	}
}

// A password too long to be anyone's is refused without being checked or counted, so it can't be
// a cheap way to fill the limiter.
func TestOverlongPasswordsAreNotCounted(t *testing.T) {
	svc, _, _, _ := accounts(t)
	ctx := context.Background()
	long := strings.Repeat("x", credential.MaxPasswordBytes+1)
	for range 40 {
		_, err := svc.Login(ctx, "owner@example.com", long, "10.0.0.1")
		wantCode(t, err, domain.CodeUnauthenticated)
	}
	if _, err := svc.Login(ctx, "owner@example.com", ownerPassword, "10.0.0.1"); err != nil {
		t.Fatalf("after over-long attempts: %v", err)
	}
}

// Attempts count from the moment they start, so guesses sent in parallel can't all be checked
// before the first failure is recorded.
func TestLoginLimitsCountAttemptsInProgress(t *testing.T) {
	svc, _, _, _ := accounts(t)
	ctx := context.Background()
	const attempts = 25
	codes := make(chan domain.Code, attempts)
	var wg sync.WaitGroup
	for range attempts {
		wg.Go(func() {
			_, err := svc.Login(ctx, "owner@example.com", "wrong password!!", "10.0.0.1")
			code, _ := domain.CodeOf(err)
			codes <- code
		})
	}
	wg.Wait()
	close(codes)
	checked := 0
	for code := range codes {
		switch code {
		case domain.CodeUnauthenticated:
			checked++
		case domain.CodeTooManyRequests:
		default:
			t.Errorf("unexpected result %q", code)
		}
	}
	if checked > 10 {
		t.Errorf("%d parallel guesses were checked; the limit is 10", checked)
	}
}

// Sign-in rehashes a password made with older parameters, so every account costs the same to check.
func TestLoginUpgradesOldHashes(t *testing.T) {
	svc, st, _, owner := accounts(t)
	ctx := context.Background()
	salt := []byte("0123456789abcdef")
	b64 := base64.RawStdEncoding
	old := fmt.Sprintf("$argon2id$v=%d$m=8192,t=1,p=1$%s$%s", argon2.Version, b64.EncodeToString(salt),
		b64.EncodeToString(argon2.IDKey([]byte(ownerPassword), salt, 1, 8192, 1, 32)))
	if !credential.NeedsRehash(old) || !credential.VerifyPassword(old, ownerPassword) {
		t.Fatal("bad fixture")
	}
	if err := st.InTx(ctx, func(q store.Queries) error { return q.SetPasswordHash(ctx, owner.ID, old) }); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Login(ctx, "owner@example.com", ownerPassword, "10.0.0.1"); err != nil {
		t.Fatal(err)
	}
	_ = st.InTx(ctx, func(q store.Queries) error {
		u, _ := q.UserByID(ctx, owner.ID)
		if credential.NeedsRehash(u.PasswordHash) || !credential.VerifyPassword(u.PasswordHash, ownerPassword) {
			t.Errorf("hash after sign-in = %.30s…; want current parameters, same password", u.PasswordHash)
		}
		return nil
	})
}

func TestDisabledAccountsCantSignIn(t *testing.T) {
	svc, st, clock, owner := accounts(t)
	ctx := context.Background()
	token, err := svc.Login(ctx, "owner@example.com", ownerPassword, "10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	disabled := owner
	disabled.ID, disabled.Email, disabled.DisabledAt = uuid.New(), "gone@example.com", clock.Now()
	disabled.PasswordHash, _ = credential.HashPassword(ownerPassword)
	if err := st.InTx(ctx, func(q store.Queries) error { return q.CreateUser(ctx, disabled) }); err != nil {
		t.Fatal(err)
	}
	_, err = svc.Login(ctx, "gone@example.com", ownerPassword, "10.0.0.1")
	wantCode(t, err, domain.CodeForbidden)
	if _, err := svc.PrincipalForSession(ctx, token); err != nil {
		t.Errorf("an active owner's session: %v", err)
	}
}

func TestChangePasswordSignsOutOtherSessions(t *testing.T) {
	svc, _, _, _ := accounts(t)
	ctx := context.Background()
	here, _ := svc.Login(ctx, "owner@example.com", ownerPassword, "10.0.0.1")
	phone, _ := svc.Login(ctx, "owner@example.com", ownerPassword, "10.0.0.2")
	p, err := svc.PrincipalForSession(ctx, here)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.CreateAPIToken(ctx, p, "laptop", 0, ownerPassword, "10.0.0.1"); err != nil {
		t.Fatal(err)
	}
	_, err = svc.ChangePassword(ctx, p, "wrong password!!", "a brand new passphrase", here, "10.0.0.1")
	wantCode(t, err, domain.CodeForbidden)
	live, err := svc.ChangePassword(ctx, p, ownerPassword, "a brand new passphrase", here, "10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if live != 1 {
		t.Errorf("live API tokens = %d, want 1: the user is told, so they can revoke them too", live)
	}
	if _, err := svc.PrincipalForSession(ctx, here); err != nil {
		t.Errorf("the session that changed the password must stay signed in: %v", err)
	}
	if _, err := svc.PrincipalForSession(ctx, phone); err == nil {
		t.Error("other sessions must be signed out")
	}
	if _, err := svc.Login(ctx, "owner@example.com", "a brand new passphrase", "10.0.0.1"); err != nil {
		t.Errorf("the new password: %v", err)
	}
}

func TestAPITokens(t *testing.T) {
	svc, st, clock, owner := accounts(t)
	ctx := context.Background()
	session, _ := svc.Login(ctx, "owner@example.com", ownerPassword, "10.0.0.1")
	p, _ := svc.PrincipalForSession(ctx, session)

	token, tok, err := svc.CreateAPIToken(ctx, p, "laptop", 24*time.Hour, ownerPassword, "10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, service.TokenPrefix) || !strings.HasPrefix(token, tok.Prefix) || tok.TokenHash == token {
		t.Errorf("token %q, stored %+v", token, tok)
	}
	got, err := svc.PrincipalForToken(ctx, token)
	if err != nil || got.UserID != owner.ID {
		t.Fatalf("token principal = %+v, %v", got, err)
	}
	if _, _, err := svc.CreateAPIToken(ctx, p, "laptop", 0, ownerPassword, "10.0.0.1"); err == nil {
		t.Error("two live tokens with one name")
	}
	if _, _, err := svc.CreateAPIToken(ctx, domain.Principal{Subject: "dev", TenantSlug: "home"}, "x", 0, ownerPassword, "10.0.0.1"); err == nil {
		t.Error("a principal without a local account made a token")
	}

	// Another user in the same tenant can't see or revoke it.
	other := domain.User{ID: uuid.New(), TenantID: owner.TenantID, Email: "other@example.com", CreatedAt: clock.Now()}
	other.PasswordHash, _ = credential.HashPassword(ownerPassword)
	if err := st.InTx(ctx, func(q store.Queries) error { return q.CreateUser(ctx, other) }); err != nil {
		t.Fatal(err)
	}
	op := domain.Principal{Subject: other.Email, TenantSlug: "home", UserID: other.ID}
	if list, err := svc.ListAPITokens(ctx, op); err != nil || len(list) != 0 {
		t.Errorf("another user's tokens = %v, %v", list, err)
	}
	wantCode(t, svc.RevokeAPIToken(ctx, op, tok.ID), domain.CodeNotFound)

	clock.Advance(25 * time.Hour)
	_, err = svc.PrincipalForToken(ctx, token)
	wantCode(t, err, domain.CodeUnauthenticated) // expired

	forever, ftok, _ := svc.CreateAPIToken(ctx, p, "ci", 0, ownerPassword, "10.0.0.1")
	if _, err := svc.PrincipalForToken(ctx, forever); err != nil {
		t.Fatal(err)
	}
	if err := svc.RevokeAPIToken(ctx, p, ftok.ID); err != nil {
		t.Fatal(err)
	}
	_, err = svc.PrincipalForToken(ctx, forever)
	wantCode(t, err, domain.CodeUnauthenticated) // revoked
	_, err = svc.PrincipalForToken(ctx, "tsk_not-a-real-token-at-all")
	wantCode(t, err, domain.CodeUnauthenticated)
}

// A stolen API token can't outlive its revocation by making more tokens or changing the password.
func TestTokensCantMakeTokensOrChangeThePassword(t *testing.T) {
	svc, _, _, _ := accounts(t)
	ctx := context.Background()
	session, _ := svc.Login(ctx, "owner@example.com", ownerPassword, "10.0.0.1")
	p, _ := svc.PrincipalForSession(ctx, session)
	token, _, err := svc.CreateAPIToken(ctx, p, "script", 0, ownerPassword, "10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	tp, err := svc.PrincipalForToken(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = svc.CreateAPIToken(ctx, tp, "another", 0, ownerPassword, "10.0.0.1")
	wantCode(t, err, domain.CodeForbidden)
	// A session needs the password too: a token outlives the session that makes it.
	_, _, err = svc.CreateAPIToken(ctx, p, "another", 0, "wrong password!!", "10.0.0.1")
	wantCode(t, err, domain.CodeForbidden)
	_, err = svc.ChangePassword(ctx, tp, ownerPassword, "a brand new passphrase", "", "10.0.0.1")
	wantCode(t, err, domain.CodeForbidden)
}

// Disabling an account ends what it already has: its sessions and tokens stop working.
func TestDisabledAccountsSessionsAndTokensStop(t *testing.T) {
	svc, st, clock, owner := accounts(t)
	ctx := context.Background()
	now := clock.Now()
	gone := domain.User{ID: uuid.New(), TenantID: owner.TenantID, Email: "gone@example.com", PasswordHash: "x",
		CreatedAt: now, DisabledAt: now}
	session, sessionHash, _ := credential.NewToken("")
	token, tokenHash, _ := credential.NewToken(service.TokenPrefix)
	err := st.InTx(ctx, func(q store.Queries) error {
		if err := q.CreateUser(ctx, gone); err != nil {
			return err
		}
		if err := q.CreateSession(ctx, domain.Session{TokenHash: sessionHash, UserID: gone.ID, CreatedAt: now,
			ExpiresAt: now.Add(time.Hour)}); err != nil {
			return err
		}
		return q.CreateAPIToken(ctx, domain.APIToken{ID: uuid.New(), UserID: gone.ID, Name: "old", Prefix: token[:8],
			TokenHash: tokenHash, CreatedAt: now})
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.PrincipalForSession(ctx, session)
	wantCode(t, err, domain.CodeUnauthenticated)
	_, err = svc.PrincipalForToken(ctx, token)
	wantCode(t, err, domain.CodeUnauthenticated)
}

// The way back in for an owner who forgot their password: einsora setup -reset-password.
func TestResetPassword(t *testing.T) {
	svc, _, _, _ := accounts(t)
	ctx := context.Background()
	session, _ := svc.Login(ctx, "owner@example.com", ownerPassword, "10.0.0.1")
	if _, err := svc.ResetPassword(ctx, "Owner@Example.com", "short"); err == nil {
		t.Error("a weak password was accepted")
	}
	_, err := svc.ResetPassword(ctx, "nobody@example.com", "a brand new passphrase")
	wantCode(t, err, domain.CodeNotFound)
	if _, err := svc.ResetPassword(ctx, "Owner@Example.com", "a brand new passphrase"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PrincipalForSession(ctx, session); err == nil {
		t.Error("a reset must sign the account out everywhere")
	}
	if _, err := svc.Login(ctx, "owner@example.com", ownerPassword, "10.0.0.1"); err == nil {
		t.Error("the old password still works")
	}
	if _, err := svc.Login(ctx, "owner@example.com", "a brand new passphrase", "10.0.0.1"); err != nil {
		t.Errorf("the new password: %v", err)
	}
}

// Timelines say which credential acted, so a leaked token's actions can be told apart (rule 14).
func TestTimelineNamesTheCredential(t *testing.T) {
	svc, _, _, _ := accounts(t)
	ctx := context.Background()
	session, _ := svc.Login(ctx, "owner@example.com", ownerPassword, "10.0.0.1")
	p, _ := svc.PrincipalForSession(ctx, session)
	token, tok, _ := svc.CreateAPIToken(ctx, p, "ci runner", 0, ownerPassword, "10.0.0.1")
	tp, _ := svc.PrincipalForToken(ctx, token)

	w, err := svc.CreateWorkload(ctx, tp, notebook("nb", 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DeleteWorkload(ctx, p, w.ID); err != nil {
		t.Fatal(err)
	}
	events, err := svc.WorkloadEvents(ctx, p, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		`created by owner@example.com (API token "ci runner" (` + tok.Prefix + `…))`,
		"delete requested by owner@example.com (browser session)",
	}
	if len(events) != 2 || events[0].Reason != want[0] || events[1].Reason != want[1] {
		t.Errorf("reasons = %+v; want %q", events, want)
	}
	if strings.Contains(events[0].Reason, token) {
		t.Error("a timeline must never hold the token itself")
	}
}

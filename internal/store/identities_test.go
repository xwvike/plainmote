package store

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func google(subject string) Identity {
	return Identity{Provider: ProviderGoogle, Subject: subject, Login: "G " + subject, Name: "G " + subject}
}

func github(subject string) Identity {
	return Identity{Provider: ProviderGitHub, Subject: subject, Login: "gh" + subject, Name: "GH " + subject}
}

func providersOf(t *testing.T, db *Store, userID string) map[string]string {
	t.Helper()
	identities, err := db.Identities(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, identity := range identities {
		got[identity.Provider] = identity.Subject
	}
	return got
}

func signIn(t *testing.T, db *Store, identity Identity) User {
	t.Helper()
	user, err := db.SignIn(context.Background(), identity)
	if err != nil {
		t.Fatal(err)
	}
	return user
}

// An identity always reaches the one account it belongs to, and a new
// identity makes a new account: nothing is matched by name.
func TestSignInKeepsEachIdentityToItsOwnAccount(t *testing.T) {
	db, alice, _ := testDatabase(t)
	ctx := context.Background()

	first := signIn(t, db, google("g-1"))
	if first.ID == alice.ID || first.GitHubID != "" || first.Name != "G g-1" {
		t.Fatalf("a Google sign-up: %+v", first)
	}
	again := signIn(t, db, Identity{Provider: ProviderGoogle, Subject: "g-1", Login: "Renamed", Name: "Renamed"})
	if again.ID != first.ID || again.Name != "Renamed" {
		t.Fatalf("a returning Google sign-in reached %+v, want %s with the new name", again, first.ID)
	}
	// Same name, another Google account: another account here.
	twin := signIn(t, db, Identity{Provider: ProviderGoogle, Subject: "g-2", Login: "Renamed", Name: "Renamed"})
	if twin.ID == first.ID {
		t.Fatal("two Google accounts with one name share an account")
	}
	// A Google subject equal to alice's GitHub ID is still someone else.
	same := signIn(t, db, google("100"))
	if same.ID == alice.ID {
		t.Fatal("a Google subject reached the account of the GitHub user with the same ID")
	}
	if got, err := db.IdentityUser(ctx, ProviderGoogle, "100"); err != nil || got.ID != same.ID {
		t.Fatalf("IdentityUser(google, 100) = %+v %v", got, err)
	}
	if got, err := db.IdentityUser(ctx, ProviderGitHub, "100"); err != nil || got.ID != alice.ID {
		t.Fatalf("IdentityUser(github, 100) = %+v %v", got, err)
	}
	if _, err := db.IdentityUser(ctx, ProviderGoogle, "unknown"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown identity: %v", err)
	}
	for _, bad := range []Identity{{Provider: "gitlab", Subject: "1"}, {Provider: ProviderGoogle, Subject: " "}} {
		if _, err := db.SignIn(ctx, bad); err == nil {
			t.Fatalf("signed in with %+v", bad)
		}
	}
}

func TestLinkingNeverMovesAnIdentityBetweenAccounts(t *testing.T) {
	db, alice, _ := testDatabase(t)
	ctx := context.Background()
	bob := signIn(t, db, google("bob"))

	if err := db.LinkIdentity(ctx, alice.ID, google("alice")); err != nil {
		t.Fatal(err)
	}
	if got := signIn(t, db, google("alice")); got.ID != alice.ID {
		t.Fatalf("the linked Google account reached %s, not alice", got.ID)
	}
	// Alice keeps her GitHub profile and github_id.
	if got := signIn(t, db, github("100")); got.ID != alice.ID || got.GitHubID != "100" || got.Name != "GH 100" {
		t.Fatalf("alice through GitHub: %+v", got)
	}
	if err := db.LinkIdentity(ctx, alice.ID, google("alice")); err != nil {
		t.Fatalf("linking what is already linked: %v", err)
	}

	if err := db.LinkIdentity(ctx, alice.ID, google("bob")); !errors.Is(err, ErrProviderLinked) && !errors.Is(err, ErrIdentityTaken) {
		t.Fatalf("linking bob's Google account to alice: %v", err)
	}
	if err := db.LinkIdentity(ctx, alice.ID, google("someone-new")); !errors.Is(err, ErrProviderLinked) {
		t.Fatalf("a second Google account for alice: %v", err)
	}
	if err := db.LinkIdentity(ctx, bob.ID, github("100")); !errors.Is(err, ErrIdentityTaken) {
		t.Fatalf("linking alice's GitHub account to bob: %v", err)
	}
	if err := db.LinkIdentity(ctx, bob.ID, google("alice")); !errors.Is(err, ErrIdentityTaken) {
		t.Fatalf("linking alice's Google account to bob: %v", err)
	}
	if got := providersOf(t, db, bob.ID); len(got) != 1 || got[ProviderGoogle] != "bob" {
		t.Fatalf("bob's identities after refused links: %v", got)
	}
	if got := providersOf(t, db, alice.ID); len(got) != 2 || got[ProviderGitHub] != "100" || got[ProviderGoogle] != "alice" {
		t.Fatalf("alice's identities after refused links: %v", got)
	}
	if got := signIn(t, db, google("bob")); got.ID != bob.ID {
		t.Fatal("a refused link moved bob's sign-in")
	}

	for _, userID := range []string{AnonymousUserID, "not-a-uuid"} {
		if err := db.LinkIdentity(ctx, userID, github("999")); !errors.Is(err, ErrNotFound) {
			t.Fatalf("linking to %q: %v", userID, err)
		}
	}
	if _, err := db.IdentityUser(ctx, ProviderGitHub, "999"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a refused link left an identity behind: %v", err)
	}
}

func TestUnlinkingLetsGoForGood(t *testing.T) {
	db, alice, _ := testDatabase(t)
	ctx := context.Background()

	if err := db.UnlinkIdentity(ctx, alice.ID, ProviderGitHub); !errors.Is(err, ErrLastIdentity) {
		t.Fatalf("unlinking the only sign-in method: %v", err)
	}
	if err := db.UnlinkIdentity(ctx, alice.ID, ProviderGoogle); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unlinking a provider alice never had: %v", err)
	}
	if err := db.LinkIdentity(ctx, alice.ID, google("alice")); err != nil {
		t.Fatal(err)
	}
	if err := db.UnlinkIdentity(ctx, alice.ID, ProviderGitHub); err != nil {
		t.Fatal(err)
	}
	if got := signIn(t, db, google("alice")); got.ID != alice.ID || got.GitHubID != "" || got.Name != "G alice" {
		t.Fatalf("alice after leaving GitHub: %+v", got)
	}
	// The GitHub account let go of reaches nothing of alice's, neither
	// through the identities nor through the github_id copy.
	if _, err := db.IdentityUser(ctx, ProviderGitHub, "100"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the unlinked GitHub account still reaches an account: %v", err)
	}
	stranger := signIn(t, db, github("100"))
	if stranger.ID == alice.ID {
		t.Fatal("the unlinked GitHub account signed in to alice")
	}
	if err := db.UnlinkIdentity(ctx, alice.ID, ProviderGoogle); !errors.Is(err, ErrLastIdentity) {
		t.Fatalf("unlinking alice's last method: %v", err)
	}
	if err := db.UnlinkIdentity(ctx, stranger.ID, ProviderGitHub); !errors.Is(err, ErrLastIdentity) {
		t.Fatalf("unlinking the stranger's only method: %v", err)
	}
	// Another account's provider cannot be unlinked through this one.
	if err := db.LinkIdentity(ctx, stranger.ID, google("stranger")); err != nil {
		t.Fatal(err)
	}
	if err := db.UnlinkIdentity(ctx, alice.ID, ProviderGitHub); !errors.Is(err, ErrNotFound) {
		t.Fatalf("alice unlinked a provider she does not have: %v", err)
	}
	if got := providersOf(t, db, stranger.ID); len(got) != 2 {
		t.Fatalf("the stranger lost an identity to alice's unlink: %v", got)
	}
}

func TestFirstSignInsAtOnceMakeOneAccount(t *testing.T) {
	db, _, _ := testDatabase(t)
	const tries = 8
	ids := make([]string, tries)
	errs := make([]error, tries)
	var wg sync.WaitGroup
	for i := range tries {
		wg.Go(func() {
			user, err := db.SignIn(context.Background(), google("racer"))
			ids[i], errs[i] = user.ID, err
		})
	}
	wg.Wait()
	for i := range tries {
		if errs[i] != nil || ids[i] != ids[0] {
			t.Fatalf("sign-in %d: %s %v, want %s", i, ids[i], errs[i], ids[0])
		}
	}
	var count int
	if err := db.db.QueryRow(context.Background(), `SELECT count(*) FROM user_identities WHERE provider = 'google' AND subject = 'racer'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("%d identities, %v", count, err)
	}
}

// An account written by a build from before identities has only its
// github_id; it is found and recorded. The anonymous account's placeholder
// github_id is no identity.
func TestLegacyGitHubAccountsAreCarriedOver(t *testing.T) {
	db, alice, _ := testDatabase(t)
	ctx := context.Background()
	if _, err := db.db.Exec(ctx, `DELETE FROM user_identities WHERE user_id = $1`, alice.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := db.IdentityUser(ctx, ProviderGitHub, "100"); err != nil || got.ID != alice.ID {
		t.Fatalf("IdentityUser before carrying over: %+v %v", got, err)
	}
	if got := signIn(t, db, github("100")); got.ID != alice.ID {
		t.Fatalf("a legacy account signed in as %s", got.ID)
	}
	if got := providersOf(t, db, alice.ID); got[ProviderGitHub] != "100" {
		t.Fatalf("the legacy identity was not recorded: %v", got)
	}

	if _, err := db.IdentityUser(ctx, ProviderGitHub, "anonymous"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the anonymous account's placeholder is an identity: %v", err)
	}
	if got, err := db.SignIn(ctx, github("anonymous")); err == nil {
		t.Fatalf("signed in with the anonymous account's placeholder, as %s", got.ID)
	}
	if err := db.LinkIdentity(ctx, alice.ID, github("anonymous")); err == nil {
		t.Fatal("linked the anonymous account's placeholder")
	}
	if err := db.LinkIdentity(ctx, alice.ID, google("x")); err != nil {
		t.Fatal(err)
	}
	if got := providersOf(t, db, AnonymousUserID); len(got) != 0 {
		t.Fatalf("the anonymous account has identities: %v", got)
	}
}

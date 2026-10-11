package store

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

const testResource = "https://cfg.test/mcp"

func pkcePair() (verifier, challenge string) {
	verifier = strings.Repeat("v", 20) + "-._~" + strings.Repeat("A", 30)
	digest := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(digest[:])
}

func TestRedirectURIs(t *testing.T) {
	for uri, want := range map[string]bool{
		"https://claude.ai/api/mcp/auth_callback":          true,
		"http://127.0.0.1:33418/callback":                  true,
		"http://localhost:6274/oauth/callback":             true,
		"http://[::1]:8080/cb":                             true,
		"cursor://anysphere.cursor-mcp/oauth/callback":     true,
		"com.example.app:/oauth2redirect":                  true,
		"http://example.com/callback":                      false,
		"http://localhost.example.com/cb":                  false,
		"https://example.com/cb#frag":                      false,
		"https://user:pass@example.com/cb":                 false,
		"javascript://example.com/%0aalert(1)":             false,
		"data:text/html,hi":                                false,
		"file:///etc/passwd":                               false,
		"myapp:callback":                                   false,
		"https:///nohost":                                  false,
		"https://example.com/\\evil":                       false,
		"https://example.com/cb\n":                         false,
		"":                                                 false,
		"https://example.com/" + strings.Repeat("a", 2000): false,
	} {
		if got := ValidRedirectURI(uri); got != want {
			t.Errorf("ValidRedirectURI(%q) = %v", uri, got)
		}
	}

	client := OAuthClient{RedirectURIs: []string{"http://127.0.0.1:33418/callback", "https://app.test/cb"}}
	for uri, want := range map[string]bool{
		"http://127.0.0.1:51234/callback": true, // any loopback port
		"http://127.0.0.1/callback":       true,
		"http://127.0.0.1:51234/other":    false,
		"http://localhost:33418/callback": false,
		"https://app.test/cb":             true,
		"https://app.test/cb?x=1":         false,
		"https://app.test:444/cb":         false,
	} {
		if got := client.RedirectURI(uri); got != want {
			t.Errorf("RedirectURI(%q) = %v", uri, got)
		}
	}
}

func TestOAuthScopes(t *testing.T) {
	for in, want := range map[string]string{
		"":                  "read",
		"write":             "read write",
		"share read":        "read share",
		"write share write": "read write share",
	} {
		if got, ok := NormalizeOAuthScope(in); !ok || got != want {
			t.Errorf("NormalizeOAuthScope(%q) = %q, %v", in, got, ok)
		}
	}
	if _, ok := NormalizeOAuthScope("read admin"); ok {
		t.Fatal("an unknown scope was accepted")
	}
}

// grantFor runs one consent and exchange, returning the tokens.
func grantFor(t *testing.T, db *Store, userID string, client OAuthClient, scope string) OAuthTokens {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	verifier, challenge := pkcePair()
	code, err := db.CreateOAuthCode(ctx, OAuthAuthorization{
		UserID: userID, Client: client, Scope: scope, Resource: testResource,
		RedirectURI: client.RedirectURIs[0], CodeChallenge: challenge,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := db.ExchangeOAuthCode(ctx, OAuthExchange{Code: code, ClientID: client.ID, RedirectURI: client.RedirectURIs[0], Verifier: verifier, Resource: testResource}, now)
	if err != nil {
		t.Fatal(err)
	}
	return tokens
}

func TestOAuthCodeExchange(t *testing.T) {
	db, alice, _ := testDatabase(t)
	ctx := context.Background()
	now := time.Now().UTC()
	client, err := db.RegisterOAuthClient(ctx, "Inspector\x00", "javascript:alert(1)", []string{"http://127.0.0.1:6274/cb"}, "203.0.113.5", now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(client.ID, OAuthClientPrefix) || client.Name != "Inspector" || client.ClientURI != "" {
		t.Fatalf("registered %+v", client)
	}
	if _, err := db.RegisterOAuthClient(ctx, "x", "", []string{"http://evil.test/cb"}, "", now); !IsRefusal(err) {
		t.Fatalf("a plain http redirect was registered: %v", err)
	}
	other, err := db.RegisterOAuthClient(ctx, "Other", "", []string{"https://other.test/cb"}, "", now)
	if err != nil {
		t.Fatal(err)
	}

	verifier, challenge := pkcePair()
	authz := OAuthAuthorization{UserID: alice.ID, Client: client, Scope: "write", Resource: testResource,
		RedirectURI: "http://127.0.0.1:50000/cb", CodeChallenge: challenge}
	if _, err := db.CreateOAuthCode(ctx, OAuthAuthorization{UserID: alice.ID, Client: client, Scope: "read", Resource: testResource,
		RedirectURI: "https://other.test/cb", CodeChallenge: challenge}, now); err == nil {
		t.Fatal("a code was made for a redirect the client does not have")
	}
	if _, err := db.CreateOAuthCode(ctx, OAuthAuthorization{UserID: alice.ID, Client: client, Scope: "read", Resource: testResource,
		RedirectURI: "http://127.0.0.1:50000/cb", CodeChallenge: "plain-challenge"}, now); err == nil {
		t.Fatal("a code was made without an S256 challenge")
	}
	code, err := db.CreateOAuthCode(ctx, authz, now)
	if err != nil {
		t.Fatal(err)
	}
	good := OAuthExchange{Code: code, ClientID: client.ID, RedirectURI: authz.RedirectURI, Verifier: verifier, Resource: testResource}

	for name, ex := range map[string]OAuthExchange{
		"another client":   {Code: code, ClientID: other.ID, RedirectURI: good.RedirectURI, Verifier: verifier},
		"another redirect": {Code: code, ClientID: client.ID, RedirectURI: "http://127.0.0.1:50001/cb", Verifier: verifier},
		"wrong verifier":   {Code: code, ClientID: client.ID, RedirectURI: good.RedirectURI, Verifier: strings.Repeat("w", 50)},
		"no verifier":      {Code: code, ClientID: client.ID, RedirectURI: good.RedirectURI},
		"another resource": {Code: code, ClientID: client.ID, RedirectURI: good.RedirectURI, Verifier: verifier, Resource: "https://cfg.test/api"},
		"unknown code":     {Code: "nope", ClientID: client.ID, RedirectURI: good.RedirectURI, Verifier: verifier},
	} {
		if _, err := db.ExchangeOAuthCode(ctx, ex, now); !errors.Is(err, ErrOAuthGrant) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// None of those spent it.
	tokens, err := db.ExchangeOAuthCode(ctx, good, now)
	if err != nil {
		t.Fatal(err)
	}
	if tokens.Scope != "read write" || !strings.HasPrefix(tokens.AccessToken, OAuthAccessPrefix) || !strings.HasPrefix(tokens.RefreshToken, OAuthRefreshPrefix) {
		t.Fatalf("tokens %+v", tokens)
	}
	user, access, err := db.OAuthAccessUser(ctx, tokens.AccessToken, "203.0.113.5", now)
	if err != nil || user.ID != alice.ID || access.Resource != testResource || !access.Has(OAuthScopeWrite) || access.Has(OAuthScopeShare) {
		t.Fatalf("access token reads as %+v %+v %v", user, access, err)
	}

	// The same code again: the grant and its tokens are ended.
	if _, err := db.ExchangeOAuthCode(ctx, good, now); !errors.Is(err, ErrOAuthGrant) {
		t.Fatalf("a code was exchanged twice: %v", err)
	}
	if _, _, err := db.OAuthAccessUser(ctx, tokens.AccessToken, "", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a token outlived its replayed code: %v", err)
	}

	// An expired code is refused.
	code, err = db.CreateOAuthCode(ctx, authz, now)
	if err != nil {
		t.Fatal(err)
	}
	good.Code = code
	if _, err := db.ExchangeOAuthCode(ctx, good, now.Add(OAuthCodeLifetime)); !errors.Is(err, ErrOAuthGrant) {
		t.Fatalf("an expired code was exchanged: %v", err)
	}
}

func TestOAuthTokensStayWithTheirOwnerAndKind(t *testing.T) {
	db, alice, _ := testDatabase(t)
	ctx := context.Background()
	now := time.Now().UTC()
	bob, err := db.UpsertUser(ctx, "200", "bob", "Bob", "")
	if err != nil {
		t.Fatal(err)
	}
	client, err := db.RegisterOAuthClient(ctx, "App", "", []string{"https://app.test/cb"}, "", now)
	if err != nil {
		t.Fatal(err)
	}
	aliceTokens := grantFor(t, db, alice.ID, client, "read")
	bobTokens := grantFor(t, db, bob.ID, client, "write share")

	if user, _, err := db.OAuthAccessUser(ctx, aliceTokens.AccessToken, "", now); err != nil || user.ID != alice.ID {
		t.Fatalf("alice's token reads as %+v", user)
	}
	if user, access, err := db.OAuthAccessUser(ctx, bobTokens.AccessToken, "", now); err != nil || user.ID != bob.ID || !access.Has(OAuthScopeShare) {
		t.Fatalf("bob's token reads as %+v %+v", user, access)
	}
	// A refresh token is not an access token, and neither is a CLI token.
	if _, _, err := db.OAuthAccessUser(ctx, aliceTokens.RefreshToken, "", now); !errors.Is(err, ErrNotFound) {
		t.Fatal("a refresh token was taken as an access token")
	}
	if _, _, err := db.TokenUser(ctx, aliceTokens.AccessToken, "", now); !errors.Is(err, ErrNotFound) {
		t.Fatal("an MCP token was taken by the command line's interface")
	}
	if _, _, err := db.OAuthAccessUser(ctx, aliceTokens.AccessToken, "", now.Add(OAuthAccessLifetime+time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Fatal("an access token outlived its hour")
	}

	// Each lists only its own; one cannot revoke the other's.
	aliceGrants, err := db.ListOAuthGrants(ctx, alice.ID, now)
	if err != nil || len(aliceGrants) != 1 || aliceGrants[0].Scope != "read" || aliceGrants[0].ClientName != "App" {
		t.Fatalf("alice's grants %+v %v", aliceGrants, err)
	}
	bobGrants, err := db.ListOAuthGrants(ctx, bob.ID, now)
	if err != nil || len(bobGrants) != 1 {
		t.Fatalf("bob's grants %+v %v", bobGrants, err)
	}
	if err := db.RevokeOAuthGrant(ctx, alice.ID, bobGrants[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("alice revoked bob's grant: %v", err)
	}
	if err := db.RevokeOAuthToken(ctx, bobTokens.RefreshToken, "pmc_someone-else"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.OAuthAccessUser(ctx, bobTokens.AccessToken, "", now); err != nil {
		t.Fatal("another client's revocation ended bob's grant")
	}
	if err := db.RevokeOAuthToken(ctx, bobTokens.RefreshToken, client.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.OAuthAccessUser(ctx, bobTokens.AccessToken, "", now); !errors.Is(err, ErrNotFound) {
		t.Fatal("a revoked refresh token left its access token working")
	}
	if err := db.RevokeOAuthGrant(ctx, alice.ID, aliceGrants[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RefreshOAuthToken(ctx, aliceTokens.RefreshToken, client.ID, "", "", now); !errors.Is(err, ErrOAuthGrant) {
		t.Fatal("a revoked grant refreshed")
	}
}

func TestOAuthRefreshRotates(t *testing.T) {
	db, alice, _ := testDatabase(t)
	ctx := context.Background()
	now := time.Now().UTC()
	client, err := db.RegisterOAuthClient(ctx, "App", "", []string{"https://app.test/cb"}, "", now)
	if err != nil {
		t.Fatal(err)
	}
	first := grantFor(t, db, alice.ID, client, "write")

	if _, err := db.RefreshOAuthToken(ctx, first.RefreshToken, "pmc_other", "", "", now); !errors.Is(err, ErrOAuthGrant) {
		t.Fatalf("another client refreshed: %v", err)
	}
	if _, err := db.RefreshOAuthToken(ctx, first.RefreshToken, client.ID, "write share", "", now); !errors.Is(err, ErrOAuthScope) {
		t.Fatalf("a refresh widened the scope: %v", err)
	}
	narrowed, err := db.RefreshOAuthToken(ctx, first.RefreshToken, client.ID, "read", testResource, now)
	if err != nil || narrowed.Scope != "read" {
		t.Fatalf("a narrowing refresh: %+v %v", narrowed, err)
	}
	if _, access, err := db.OAuthAccessUser(ctx, narrowed.AccessToken, "", now); err != nil || access.Has(OAuthScopeWrite) {
		t.Fatalf("the narrowed token: %+v %v", access, err)
	}

	// The refresh slides: thirty days from the last use, not the first.
	later := now.Add(OAuthRefreshLifetime - time.Hour)
	second, err := db.RefreshOAuthToken(ctx, narrowed.RefreshToken, client.ID, "", "", later)
	if err != nil || second.Scope != "read write" {
		t.Fatalf("a later refresh: %+v %v", second, err)
	}
	grants, err := db.ListOAuthGrants(ctx, alice.ID, now.Add(OAuthRefreshLifetime+time.Hour))
	if err != nil || len(grants) != 1 {
		t.Fatalf("the grant did not slide: %+v %v", grants, err)
	}

	// A spent refresh token presented again ends the grant for both holders.
	if _, err := db.RefreshOAuthToken(ctx, first.RefreshToken, client.ID, "", "", later); !errors.Is(err, ErrOAuthGrant) {
		t.Fatalf("a spent refresh token worked: %v", err)
	}
	if _, err := db.RefreshOAuthToken(ctx, second.RefreshToken, client.ID, "", "", later); !errors.Is(err, ErrOAuthGrant) {
		t.Fatal("the grant survived a replayed refresh token")
	}
	if _, _, err := db.OAuthAccessUser(ctx, second.AccessToken, "", later); !errors.Is(err, ErrNotFound) {
		t.Fatal("an access token survived a replayed refresh token")
	}
}

func TestSuspensionEndsOAuthGrants(t *testing.T) {
	db, alice, _ := testDatabase(t)
	ctx := context.Background()
	now := time.Now().UTC()
	client, err := db.RegisterOAuthClient(ctx, "App", "", []string{"https://app.test/cb"}, "", now)
	if err != nil {
		t.Fatal(err)
	}
	tokens := grantFor(t, db, alice.ID, client, "write")
	if err := db.AdminSuspendUser(ctx, AdminActor{KeyID: "test"}, alice.ID, "test", now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.OAuthAccessUser(ctx, tokens.AccessToken, "", now); !errors.Is(err, ErrNotFound) {
		t.Fatal("a suspended account's access token works")
	}
	if _, err := db.RefreshOAuthToken(ctx, tokens.RefreshToken, client.ID, "", "", now); !errors.Is(err, ErrOAuthGrant) {
		t.Fatal("a suspended account's refresh token works")
	}
	_, challenge := pkcePair()
	if _, err := db.CreateOAuthCode(ctx, OAuthAuthorization{UserID: alice.ID, Client: client, Resource: testResource,
		RedirectURI: "https://app.test/cb", CodeChallenge: challenge}, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a suspended account consented: %v", err)
	}
}

func TestDocumentClientsKeepTheirKind(t *testing.T) {
	db, _, _ := testDatabase(t)
	ctx := context.Background()
	now := time.Now().UTC()
	id := "https://app.test/oauth/client.json"
	if _, err := db.SaveOAuthDocumentClient(ctx, id, "App", "https://app.test", []string{"https://app.test/cb"}, now); err != nil {
		t.Fatal(err)
	}
	updated, err := db.SaveOAuthDocumentClient(ctx, id, "App 2", "", []string{"https://app.test/cb2"}, now.Add(time.Hour))
	if err != nil || updated.Name != "App 2" || !updated.RedirectURI("https://app.test/cb2") || updated.RedirectURI("https://app.test/cb") {
		t.Fatalf("refetched document %+v %v", updated, err)
	}
	registered, err := db.RegisterOAuthClient(ctx, "Reg", "", []string{"https://reg.test/cb"}, "", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SaveOAuthDocumentClient(ctx, registered.ID, "Hijack", "", []string{"https://evil.test/cb"}, now); err == nil {
		t.Fatal("a document overwrote a registered client")
	}
	if got, err := db.OAuthClient(ctx, registered.ID); err != nil || got.RedirectURI("https://evil.test/cb") {
		t.Fatalf("registered client now %+v", got)
	}
}

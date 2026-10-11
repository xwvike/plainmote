package web

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"plainmote/internal/store"
)

// A token is turned away from a tool beyond its scope with the challenge
// that lets its client ask for more, before the tool does anything.
func TestMCPToolsNeedTheirScope(t *testing.T) {
	db, alice, example := testDatabase(t)
	app := newTestApp(db)
	ctx := context.Background()
	read := mcpToken(t, db, alice, "read", testMCPResource).AccessToken
	write := mcpToken(t, db, alice, "read write", testMCPResource).AccessToken
	for _, c := range []struct {
		token, tool, wants string
		args               map[string]any
	}{
		{read, "create_resource", "read write", map[string]any{"content": "x"}},
		{read, "update_resource", "read write", map[string]any{"resource": example.ID, "base_version": 1, "content": "x"}},
		{read, "share_resource", "read share", map[string]any{"resource": example.ID}},
		{write, "share_resource", "read write share", map[string]any{"resource": example.ID}},
		{write, "unshare_resource", "read write share", map[string]any{"resource": example.ID}},
	} {
		response, answer := mcpRequest{token: c.token, version: mcpModernVersion, modern: true, method: "tools/call",
			params: map[string]any{"name": c.tool, "arguments": c.args}}.send(app)
		challenge := response.Header().Get("WWW-Authenticate")
		if response.Code != http.StatusForbidden || rpcCode(answer) != rpcInsufficientScope ||
			!strings.Contains(challenge, `error="insufficient_scope"`) || !strings.Contains(challenge, `scope="`+c.wants+`"`) {
			t.Errorf("%s with %q: %d %q", c.tool, c.wants, response.Code, challenge)
		}
	}
	resources, _, err := db.ListResources(ctx, alice.ID, "", 10, 0)
	if err != nil || len(resources) != 1 || resources[0].Version != 1 {
		t.Fatalf("a refused call changed something: %v %v", resources, err)
	}
	if links, err := db.ListShares(ctx, alice.ID, example.ID, time.Now().UTC()); err != nil || len(links) != 0 {
		t.Fatalf("a refused call made a link: %v %v", links, err)
	}
}

func TestMCPWriteTools(t *testing.T) {
	db, alice, example := testDatabase(t)
	app := newTestApp(db)
	ctx := context.Background()
	token := mcpToken(t, db, alice, "read write", testMCPResource).AccessToken

	created := toolCall(t, app, token, "create_resource", map[string]any{"content": "a: 1\n", "filename": "app.yaml"})
	id, _ := created.structured["id"].(string)
	if created.isError || id == "" || created.structured["name"] != "app.yaml" || created.structured["version"].(float64) != 1 {
		t.Fatalf("create: %v %v", created.texts, created.structured)
	}
	if read := toolCall(t, app, token, "read_resource", map[string]any{"resource": id}); read.structured["text"] != "a: 1\n" {
		t.Fatalf("read back: %v", read.structured)
	}
	for name, args := range map[string]map[string]any{
		"no content":    {"filename": "x.txt"},
		"empty content": {"content": ""},
		"too large":     {"content": strings.Repeat("x", int(app.cfg.MaxContent)+1)},
		"long name":     {"content": "x", "name": strings.Repeat("n", 201)},
		"bad filename":  {"content": "x", "filename": "../etc/passwd"},
	} {
		if refused := toolCall(t, app, token, "create_resource", args); !refused.isError {
			t.Errorf("%s: created", name)
		}
	}

	if missing := toolCall(t, app, token, "update_resource", map[string]any{"resource": id, "content": "a: 2\n"}); !missing.isError {
		t.Fatal("a save without base_version was taken")
	}
	saved := toolCall(t, app, token, "update_resource", map[string]any{"resource": id, "base_version": 1, "content": "a: 2\n"})
	if saved.isError || saved.structured["version"].(float64) != 2 || saved.structured["new_version"] != true {
		t.Fatalf("save: %v %v", saved.texts, saved.structured)
	}
	// Saved from version 1 again: someone else's version 2 is in the way.
	stale := toolCall(t, app, token, "update_resource", map[string]any{"resource": id, "base_version": 1, "content": "a: 3\n"})
	if !stale.isError || !strings.Contains(stale.texts[0], "version 2") {
		t.Fatalf("a stale save: %v", stale.texts)
	}
	if read := toolCall(t, app, token, "read_resource", map[string]any{"resource": id}); read.structured["text"] != "a: 2\n" {
		t.Fatalf("a refused save changed the text: %v", read.structured["text"])
	}
	if same := toolCall(t, app, token, "update_resource", map[string]any{"resource": id, "base_version": 2, "content": "a: 2\n"}); same.isError || same.structured["new_version"] != false {
		t.Fatalf("the same text again: %v", same.structured)
	}

	// A file with Windows line endings keeps them.
	crlf, err := db.CreateResource(ctx, alice.ID, "", "win.ini", []byte("[a]\r\nb=1\r\n"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	toolCall(t, app, token, "update_resource", map[string]any{"resource": crlf.ID, "base_version": 1, "content": "[a]\nb=2\n"})
	if after, err := db.ResourceForOwner(ctx, alice.ID, crlf.ID); err != nil {
		t.Fatal(err)
	} else if stored, _ := db.ReadContent(ctx, after); string(stored) != "[a]\r\nb=2\r\n" {
		t.Fatalf("line endings: %q", stored)
	}

	// What cannot take text from here is refused and left as it was.
	sealedID := uuid.NewString()
	if _, err := db.CreateSealedResource(ctx, alice.ID, sealedID, store.SealedPart{
		Content: append([]byte("PMr1"), bytes.Repeat([]byte("S"), 48)...), Meta: append([]byte("PMm1"), bytes.Repeat([]byte("M"), 40)...),
	}, bytes.Repeat([]byte{9}, 60)); err != nil {
		t.Fatal(err)
	}
	quick, _, err := db.CreateQuickShare(ctx, alice.ID, "q.txt", []byte("quick\n"), time.Hour, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	logo, err := db.CreateResource(ctx, alice.ID, "Logo", "logo.png", append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...), "", "")
	if err != nil {
		t.Fatal(err)
	}
	for name, ref := range map[string]string{"encrypted": sealedID, "quick share": quick.ID, "binary": logo.ID} {
		refused := toolCall(t, app, token, "update_resource", map[string]any{"resource": ref, "base_version": 1, "content": "plain\n"})
		after, err := db.ResourceForOwner(ctx, alice.ID, ref)
		if !refused.isError || err != nil || after.Version != 1 {
			t.Errorf("%s: %v version %d", name, refused.texts, after.Version)
		}
	}
	if after, _ := db.ResourceForOwner(ctx, alice.ID, example.ID); after.Version != 1 {
		t.Fatal("an untouched resource changed")
	}
}

func TestMCPShareTools(t *testing.T) {
	db, alice, example := testDatabase(t)
	client := signedIn(t, db, alice)
	app := client.app
	ctx := context.Background()
	token := mcpToken(t, db, alice, "read share", testMCPResource).AccessToken

	shared := toolCall(t, app, token, "share_resource", map[string]any{"resource": example.ID, "name": "for bob"})
	address, _ := shared.structured["url"].(string)
	if shared.isError || !strings.HasPrefix(address, "https://cfg.test/d/") || !strings.Contains(shared.texts[0], "Anyone with this address") {
		t.Fatalf("share: %v %v", shared.texts, shared.structured)
	}
	links, err := db.ListShares(ctx, alice.ID, example.ID, time.Now().UTC())
	if err != nil || len(links) != 1 || links[0].CreatedByApp != "Test App" || links[0].Name != "for bob" ||
		links[0].ExpiresAt == nil || links[0].ExpiresAt.Sub(time.Now()) < 6*24*time.Hour || links[0].ExpiresAt.Sub(time.Now()) > 7*24*time.Hour {
		t.Fatalf("the link: %+v %v", links, err)
	}
	// The address works for anyone, and the owner's page says who made it.
	if opened := call(app, http.MethodGet, strings.TrimPrefix(address, "https://cfg.test"), "", ""); opened.Code != http.StatusOK || opened.Body.String() != "answer=42\n" {
		t.Fatalf("open the link: %d", opened.Code)
	}
	page := client.do(http.MethodGet, "/resources/"+example.ID, nil).Body.String()
	if !strings.Contains(page, `class="via"`) || !strings.Contains(page, "Test App") {
		t.Fatal("the resource page does not say an app made the link")
	}
	settings := client.do(http.MethodGet, "/resources/"+example.ID+"?share="+links[0].ID, nil).Body.String()
	if !strings.Contains(settings, "Test App · ") {
		t.Fatal("the link's settings do not name the app")
	}

	forever := toolCall(t, app, token, "share_resource", map[string]any{"resource": example.ID, "expires_in": "never", "max_uses": 2})
	if forever.isError || forever.structured["expires_at"] != nil || forever.structured["max_uses"].(float64) != 2 {
		t.Fatalf("a lasting link: %v", forever.structured)
	}
	for name, args := range map[string]map[string]any{
		"too long":     {"resource": example.ID, "expires_in": "400d"},
		"not duration": {"resource": example.ID, "expires_in": "soon"},
		"negative use": {"resource": example.ID, "max_uses": -1},
	} {
		if refused := toolCall(t, app, token, "share_resource", args); !refused.isError {
			t.Errorf("%s: shared", name)
		}
	}

	// By address, then by id; then everything left at once.
	if gone := toolCall(t, app, token, "unshare_resource", map[string]any{"resource": example.ID, "link": address}); gone.isError || gone.structured["revoked"].(float64) != 1 {
		t.Fatalf("unshare by address: %v", gone.texts)
	}
	if opened := call(app, http.MethodGet, strings.TrimPrefix(address, "https://cfg.test"), "", ""); opened.Code == http.StatusOK {
		t.Fatal("a revoked link still opens")
	}
	if again := toolCall(t, app, token, "unshare_resource", map[string]any{"resource": example.ID, "link": address}); !again.isError {
		t.Fatal("a revoked link was revoked again")
	}
	if gone := toolCall(t, app, token, "unshare_resource", map[string]any{"resource": example.ID, "link": forever.structured["link_id"]}); gone.isError {
		t.Fatalf("unshare by id: %v", gone.texts)
	}
	toolCall(t, app, token, "share_resource", map[string]any{"resource": example.ID})
	toolCall(t, app, token, "share_resource", map[string]any{"resource": example.ID})
	if all := toolCall(t, app, token, "unshare_resource", map[string]any{"resource": example.ID}); all.isError || all.structured["revoked"].(float64) != 2 {
		t.Fatalf("unshare all: %v", all.texts)
	}
	if links, _ := db.ListShares(ctx, alice.ID, example.ID, time.Now().UTC()); len(links) != 0 {
		t.Fatalf("links left: %d", len(links))
	}

	sealedID := uuid.NewString()
	if _, err := db.CreateSealedResource(ctx, alice.ID, sealedID, store.SealedPart{
		Content: append([]byte("PMr1"), bytes.Repeat([]byte("S"), 48)...), Meta: append([]byte("PMm1"), bytes.Repeat([]byte("M"), 40)...),
	}, bytes.Repeat([]byte{9}, 60)); err != nil {
		t.Fatal(err)
	}
	if refused := toolCall(t, app, token, "share_resource", map[string]any{"resource": sealedID}); !refused.isError || refused.structured != nil {
		t.Fatalf("an encrypted resource was shared: %v", refused)
	}
	if links, _ := db.ListShares(ctx, alice.ID, sealedID, time.Now().UTC()); len(links) != 0 {
		t.Fatal("an encrypted resource has a link")
	}

	// One authorization makes a limited number of links an hour.
	busy := mcpToken(t, db, alice, "read share", testMCPResource).AccessToken
	for i := 0; i < 30; i++ {
		if made := toolCall(t, app, busy, "share_resource", map[string]any{"resource": example.ID}); made.isError {
			t.Fatalf("link %d: %v", i, made.texts)
		}
	}
	if limited := toolCall(t, app, busy, "share_resource", map[string]any{"resource": example.ID}); !limited.isError {
		t.Fatal("the 31st link in an hour was made")
	}
}

// One account's write and share tokens, pointed at another account's
// resource, change nothing and learn nothing.
func TestMCPWritesStayWithinTheAccount(t *testing.T) {
	db, alice, example := testDatabase(t)
	app := newTestApp(db)
	ctx := context.Background()
	bob, err := db.UpsertUser(ctx, "200", "bob", "Bob", "")
	if err != nil {
		t.Fatal(err)
	}
	alicesLink, err := db.CreateShare(ctx, alice.ID, example.ID, "", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	token := mcpToken(t, db, bob, "read write share", testMCPResource).AccessToken
	ghost := uuid.NewString()
	for _, c := range []struct {
		tool string
		args func(ref string) map[string]any
	}{
		{"update_resource", func(ref string) map[string]any {
			return map[string]any{"resource": ref, "base_version": 1, "content": "taken\n"}
		}},
		{"share_resource", func(ref string) map[string]any { return map[string]any{"resource": ref} }},
		{"unshare_resource", func(ref string) map[string]any { return map[string]any{"resource": ref} }},
	} {
		for _, pair := range [][2]string{
			{example.ID, ghost},
			{"https://cfg.test/resources/" + example.ID, "https://cfg.test/resources/" + ghost},
			{"https://cfg.test" + shareAddress(alicesLink.Token, "x"), "https://cfg.test" + shareAddress(strings.Repeat("A", len(alicesLink.Token)), "x")},
		} {
			alices := toolCall(t, app, token, c.tool, c.args(pair[0]))
			nobodys := toolCall(t, app, token, c.tool, c.args(pair[1]))
			if !alices.isError || strings.ReplaceAll(alices.texts[0], pair[0], "REF") != strings.ReplaceAll(nobodys.texts[0], pair[1], "REF") {
				t.Fatalf("%s %s: %q vs %q", c.tool, pair[0], alices.texts, nobodys.texts)
			}
		}
	}
	after, err := db.ResourceForOwner(ctx, alice.ID, example.ID)
	if err != nil || after.Version != 1 {
		t.Fatalf("alice's resource changed: %v", err)
	}
	links, err := db.ListShares(ctx, alice.ID, example.ID, time.Now().UTC())
	if err != nil || len(links) != 1 || links[0].ID != alicesLink.ID || links[0].UsedCount != 0 {
		t.Fatalf("alice's links changed: %+v %v", links, err)
	}
}

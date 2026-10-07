package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"plainmote/internal/store"
)

// node runs one of the web package's test scripts with the browser's own
// modules, so what this command line writes is checked against the code
// that reads it in a browser, and the other way round.
func node(t *testing.T, script string, env ...string) []byte {
	t.Helper()
	binary, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	web, _ := filepath.Abs(filepath.Join("..", "..", "internal", "web"))
	source, err := os.ReadFile(filepath.Join(web, "testdata", script))
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary, "--input-type=module", "-e", string(source))
	command.Env = append(os.Environ(),
		"KEYS_MODULE=file://"+filepath.Join(web, "static", "keys.js"),
		"SEAL_MODULE=file://"+filepath.Join(web, "static", "seal.js"))
	command.Env = append(command.Env, env...)
	out, err := command.Output()
	if err != nil {
		var stderr []byte
		if exit, ok := err.(*exec.ExitError); ok {
			stderr = exit.Stderr
		}
		t.Fatalf("%s: %v\n%s", script, err, stderr)
	}
	return out
}

// sealedHarness is an account with a master password and one encrypted
// resource of two versions, both made by the browser's code.
func sealedHarness(t *testing.T) (*harness, string, map[string]string) {
	h := newHarness(t)
	h.signIn("write")
	ctx := context.Background()
	id := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	var made struct {
		Keyring   map[string]string `json:"keyring"`
		SealedKey string            `json:"sealed_key"`
		V1, V2    struct{ Content, Meta string }
	}
	if err := json.Unmarshal(node(t, "make_sealed.mjs", "USER_ID="+h.user.ID, "RESOURCE_ID="+id), &made); err != nil {
		t.Fatal(err)
	}
	decode := func(s string) []byte { b, _ := base64.RawURLEncoding.DecodeString(s); return b }
	if _, err := h.db.CreateKeyring(ctx, h.user.ID, store.Keyring{KDF: made.Keyring["kdf"], Iterations: 600000, Salt: decode(made.Keyring["salt"]),
		WrappedByPassword: decode(made.Keyring["wrapped_by_password"]), WrappedByRecovery: decode(made.Keyring["wrapped_by_recovery"])}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.CreateSealedResource(ctx, h.user.ID, id, store.SealedPart{Content: decode(made.V1.Content), Meta: decode(made.V1.Meta)}, decode(made.SealedKey)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.SaveSealedResource(ctx, h.user.ID, id, 1, store.SealedPart{Content: decode(made.V2.Content), Meta: decode(made.V2.Meta)}); err != nil {
		t.Fatal(err)
	}
	return h, id, made.Keyring
}

// What was saved through this command line, opened by the browser's code.
func openInBrowser(t *testing.T, h *harness, keyring map[string]string, id string) (string, map[string]string) {
	t.Helper()
	ctx := context.Background()
	resource, err := h.db.ResourceForOwner(ctx, h.user.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	body, err := h.db.ReadContent(ctx, resource)
	if err != nil {
		t.Fatal(err)
	}
	ring, _ := json.Marshal(map[string]any{"kdf": keyring["kdf"], "iterations": 600000, "salt": keyring["salt"], "wrapped_by_password": keyring["wrapped_by_password"]})
	encode := base64.RawURLEncoding.EncodeToString
	var opened struct {
		Meta map[string]string `json:"meta"`
		Text string            `json:"text"`
	}
	out := node(t, "open_sealed.mjs", "KEYRING="+string(ring), "USER_ID="+h.user.ID, "RESOURCE_ID="+id,
		"SEALED_KEY="+encode(resource.SealedKey), "SEALED_META="+encode(resource.SealedMeta), "CONTENT="+encode(body))
	if err := json.Unmarshal(out, &opened); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	return opened.Text, opened.Meta
}

func TestEncryptedResourcesFromTheCommandLine(t *testing.T) {
	h, id, keyring := sealedHarness(t)

	// Without the password, and nobody to ask: said so, nothing shown.
	if exit, out, errOut := h.run(nil, "cat", id[:8]); exit != 1 || out != "" || !strings.Contains(errOut, "PLAINMOTE_MASTER_PASSWORD") {
		t.Fatalf("cat without the password: %d %q %s", exit, out, errOut)
	}
	h.env = map[string]string{"PLAINMOTE_MASTER_PASSWORD": "wrong horse"}
	if exit, _, errOut := h.run(nil, "cat", id[:8]); exit != 1 || !strings.Contains(errOut, "incorrect") {
		t.Fatalf("cat with a wrong password: %d %s", exit, errOut)
	}
	h.env = nil
	asked := 0
	prompt := func(string) (string, error) { asked++; return "correct horse", nil }
	if exit, out, _ := h.run(&cli{readPassword: prompt}, "cat", id[:8]); exit != 0 || out != "SECRET=2\n" || asked != 1 {
		t.Fatalf("cat: %d %q asked %d", exit, out, asked)
	}

	// ls says it is encrypted without asking; --decrypt asks and matches the
	// real name.
	if exit, out, _ := h.run(&cli{readPassword: func(string) (string, error) { t.Fatal("ls asked for the password"); return "", nil }}, "ls"); exit != 0 || !strings.Contains(out, "(encrypted)") || strings.Contains(out, ".env") {
		t.Fatalf("ls: %d %s", exit, out)
	}
	h.env = map[string]string{"PLAINMOTE_MASTER_PASSWORD": "correct horse"}
	if exit, out, _ := h.run(nil, "ls", "--decrypt", "生产"); exit != 0 || !strings.Contains(out, ".env") || !strings.Contains(out, id[:8]) {
		t.Fatalf("ls --decrypt: %d %s", exit, out)
	}
	if exit, out, _ := h.run(nil, "ls", "--decrypt", "nothing-like-it"); exit != 0 || strings.Contains(out, id[:8]) {
		t.Fatalf("ls --decrypt with another keyword: %d %s", exit, out)
	}

	// An edit is decrypted into a file here and encrypted again on the way
	// out; the browser's code reads it back.
	editor := func(command []string) error {
		file := command[len(command)-1]
		body, _ := os.ReadFile(file)
		if string(body) != "SECRET=2\n" {
			return errors.New("the editor was given " + string(body))
		}
		return os.WriteFile(file, []byte("SECRET=3\n"), 0o600)
	}
	if exit, out, errOut := h.run(&cli{runEditor: editor}, "edit", id[:8]); exit != 0 || !strings.Contains(out, "v3") {
		t.Fatalf("edit: %d %s %s", exit, out, errOut)
	}
	if text, meta := openInBrowser(t, h, keyring, id); text != "SECRET=3\n" || meta["filename"] != ".env" || meta["name"] != "生产" {
		t.Fatalf("after the edit, the browser reads %q %v", text, meta)
	}

	// push --to encrypts under the same key; push --encrypt makes a new one.
	file := filepath.Join(t.TempDir(), "next.env")
	if err := os.WriteFile(file, []byte("SECRET=4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if exit, out, _ := h.run(nil, "push", file, "--to", id[:8]); exit != 0 || !strings.Contains(out, "v4") {
		t.Fatalf("push --to: %d %s", exit, out)
	}
	if text, _ := openInBrowser(t, h, keyring, id); text != "SECRET=4\n" {
		t.Fatalf("after push --to, the browser reads %q", text)
	}
	exit, out, errOut := h.run(&cli{stdin: strings.NewReader("token=abc\n")}, "push", "-", "--name", "token", "--filename", "token.txt", "--encrypt")
	if exit != 0 || !strings.Contains(out, "end-to-end encrypted") {
		t.Fatalf("push --encrypt: %d %s %s", exit, out, errOut)
	}
	created := strings.Fields(out)[len(strings.Fields(out))-1]
	newID := created[strings.LastIndex(created, "/")+1:]
	stored, err := h.db.ResourceForOwner(context.Background(), h.user.ID, newID)
	if err != nil || !stored.Sealed() || stored.Name != "" {
		t.Fatalf("the new resource: %+v %v", stored, err)
	}
	if body, _ := h.db.ReadContent(context.Background(), stored); bytes.Contains(body, []byte("token=abc")) {
		t.Fatal("the new resource holds plaintext")
	}
	if text, meta := openInBrowser(t, h, keyring, newID); text != "token=abc\n" || meta["name"] != "token" || !strings.HasPrefix(meta["type"], "text/plain") {
		t.Fatalf("the browser reads the new resource as %q %v", text, meta)
	}
}

// An encrypted quick share made here opens in a recipient's browser with
// the key after # alone, and its owner's browser gets that key back.
func TestEncryptedQuickShareFromTheCommandLine(t *testing.T) {
	h, _, keyring := sealedHarness(t)
	h.env = map[string]string{"PLAINMOTE_MASTER_PASSWORD": "correct horse"}
	exit, out, errOut := h.run(&cli{stdin: strings.NewReader("boot ok\n")}, "share", "-", "--filename", "app.log", "--ttl", "1h", "--encrypt")
	if exit != 0 || !strings.Contains(out, "#k=") {
		t.Fatalf("share --encrypt: %d %q %s", exit, out, errOut)
	}
	address := strings.TrimSpace(out)
	link, key, _ := strings.Cut(address, "#k=")
	response, err := http.Get(link)
	if err != nil {
		t.Fatal(err)
	}
	bundle, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || bytes.Contains(bundle, []byte("boot ok")) || !bytes.HasPrefix(bundle, []byte("PMs1")) {
		t.Fatalf("delivered: %d %q", response.StatusCode, bundle)
	}

	ctx := context.Background()
	resources, _, _ := h.db.ListResources(ctx, h.user.ID, "", 50, 0)
	var made store.Resource
	for _, r := range resources {
		if r.QuickShare() {
			made = r
		}
	}
	if !made.Sealed() || made.Filename != "" {
		t.Fatalf("stored: %+v", made)
	}
	shares, _ := h.db.ListShares(ctx, h.user.ID, made.ID, time.Now().UTC())
	if len(shares) != 1 {
		t.Fatalf("links: %d", len(shares))
	}
	ring, _ := json.Marshal(map[string]any{"kdf": keyring["kdf"], "iterations": 600000, "salt": keyring["salt"], "wrapped_by_password": keyring["wrapped_by_password"]})
	encode := base64.RawURLEncoding.EncodeToString
	var opened struct {
		Meta  map[string]string `json:"meta"`
		Text  string            `json:"text"`
		Owner bool              `json:"owner"`
	}
	result := node(t, "open_bundle.mjs", "BUNDLE="+encode(bundle), "KEY="+key, "KEYRING="+string(ring), "USER_ID="+h.user.ID,
		"LINK_ID="+shares[0].ID, "OWNER_KEY="+encode(shares[0].OwnerKey))
	if err := json.Unmarshal(result, &opened); err != nil {
		t.Fatalf("%v: %s", err, result)
	}
	if opened.Text != "boot ok\n" || opened.Meta["filename"] != "app.log" || !strings.HasPrefix(opened.Meta["type"], "text/") || !opened.Owner {
		t.Fatalf("the browser opens %+v", opened)
	}
}

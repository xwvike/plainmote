package web

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"plainmote/internal/store"
)

func runNode(t *testing.T, script string, env ...string) []byte {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	source, err := os.ReadFile(filepath.Join("testdata", script))
	if err != nil {
		t.Fatal(err)
	}
	modules := []string{}
	for _, name := range []string{"keys", "seal", "export", "zip"} {
		path, _ := filepath.Abs(filepath.Join("static", name+".js"))
		modules = append(modules, strings.ToUpper(name)+"_MODULE=file://"+path)
	}
	command := exec.Command(node, "--input-type=module", "-e", string(source))
	command.Env = append(append(os.Environ(), modules...), env...)
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

// An account with an encrypted resource exports it as ciphertext with what
// opens it beside it; the account page, unlocked, turns that archive into
// one with the plaintext under the names it really has.
func TestExportDecryptsInTheBrowser(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	id := uuid.NewString()
	var made struct {
		Keyring   map[string]string `json:"keyring"`
		SealedKey string            `json:"sealed_key"`
		V1, V2    struct{ Content, Meta string }
	}
	if err := json.Unmarshal(runNode(t, "make_sealed.mjs", "USER_ID="+user.ID, "RESOURCE_ID="+id), &made); err != nil {
		t.Fatal(err)
	}
	decode := func(s string) []byte { b, _ := base64.RawURLEncoding.DecodeString(s); return b }
	if _, err := db.CreateKeyring(ctx, user.ID, store.Keyring{KDF: made.Keyring["kdf"], Iterations: 600000, Salt: decode(made.Keyring["salt"]),
		WrappedByPassword: decode(made.Keyring["wrapped_by_password"]), WrappedByRecovery: decode(made.Keyring["wrapped_by_recovery"])}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateSealedResource(ctx, user.ID, id, store.SealedPart{Content: decode(made.V1.Content), Meta: decode(made.V1.Meta)}, decode(made.SealedKey)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SaveSealedResource(ctx, user.ID, id, 1, store.SealedPart{Content: decode(made.V2.Content), Meta: decode(made.V2.Meta)}); err != nil {
		t.Fatal(err)
	}

	client := newVersionClient(t, db, user)
	exported := client.do(http.MethodPost, "/account/export", url.Values{})
	if exported.Code != http.StatusOK {
		t.Fatalf("export: %d", exported.Code)
	}
	archive := exported.Body.Bytes()
	files := readArchive(t, archive)
	if !bytes.Equal(files["files/"+id+"/content.sealed"], decode(made.V2.Content)) || !bytes.Equal(files["files/"+id+"/versions/v1/content.sealed"], decode(made.V1.Content)) {
		t.Fatal("the server's archive must hold the ciphertext as stored")
	}
	if !strings.Contains(string(files["sealed.json"]), id) || !strings.Contains(string(files["sealed.json"]), made.Keyring["wrapped_by_password"]) {
		t.Fatalf("sealed.json: %s", files["sealed.json"])
	}
	for name, body := range files {
		if bytes.Contains(body, []byte("SECRET=")) {
			t.Fatalf("%s holds plaintext", name)
		}
	}
	page := client.do(http.MethodGet, "/account", nil).Body.String()
	if !strings.Contains(page, `data-sealed="1"`) || !strings.Contains(page, assetPath("export.js")) {
		t.Fatal("the account page must offer to decrypt the export")
	}

	dir := t.TempDir()
	in, out := filepath.Join(dir, "in.zip"), filepath.Join(dir, "out.zip")
	if err := os.WriteFile(in, archive, 0o600); err != nil {
		t.Fatal(err)
	}
	ring, _ := json.Marshal(map[string]any{"kdf": made.Keyring["kdf"], "iterations": 600000, "salt": made.Keyring["salt"],
		"wrapped_by_password": made.Keyring["wrapped_by_password"], "wrapped_by_recovery": made.Keyring["wrapped_by_recovery"]})
	runNode(t, "export_test.mjs", "USER_ID="+user.ID, "ARCHIVE="+in, "OUT="+out, "KEYRING="+string(ring))
	result, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	decrypted := readArchive(t, result)
	if string(decrypted["files/"+id+"/.env"]) != "SECRET=2\n" || string(decrypted["files/"+id+"/versions/v1/old.env"]) != "SECRET=1\n" {
		t.Fatalf("decrypted archive: %v", keys(decrypted))
	}
	for name := range decrypted {
		if strings.HasSuffix(name, ".sealed") || name == "sealed.json" {
			t.Fatalf("%s left in the decrypted archive", name)
		}
	}
	if _, ok := decrypted["access_logs.json"]; !ok {
		t.Fatal("the rest of the archive must be carried over")
	}
}

func readArchive(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for _, file := range reader.File {
		body, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		files[file.Name], _ = io.ReadAll(body)
		body.Close()
	}
	return files
}

func keys(m map[string][]byte) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	return out
}

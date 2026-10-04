package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"plainmote/internal/auth"
	"plainmote/internal/store"
	"plainmote/internal/testsupport"
	"plainmote/internal/upstream"
	"plainmote/internal/web"
)

func TestNormalizeServer(t *testing.T) {
	for in, want := range map[string]string{
		"plainmote.link":           "https://plainmote.link",
		"https://plainmote.link/":  "https://plainmote.link",
		"https://example.com:8443": "https://example.com:8443",
		"http://localhost:8964":    "http://localhost:8964",
		"http://127.0.0.1:8964":    "http://127.0.0.1:8964",
		"http://example.com":       "",
		"https://user@example.com": "",
		"https://example.com/path": "",
		"https://example.com/?x=1": "",
		"ftp://example.com":        "",
		"":                         "",
	} {
		got, err := normalizeServer(in)
		if (want == "") != (err != nil) || got != want {
			t.Errorf("normalizeServer(%q) = %q, %v", in, got, err)
		}
	}
}

func TestEditorCommand(t *testing.T) {
	env := func(values map[string]string) func(string) string {
		return func(name string) string { return values[name] }
	}
	for _, tc := range []struct {
		values map[string]string
		want   string
	}{
		{map[string]string{"EDITOR": "code"}, "code --wait"},
		{map[string]string{"EDITOR": "zed"}, "zed --wait"},
		{map[string]string{"EDITOR": "subl -w"}, "subl -w"},
		{map[string]string{"VISUAL": "nvim", "EDITOR": "code"}, "nvim"},
		{map[string]string{"EDITOR": `"/Applications/Visual Studio Code.app/bin/code" --new-window`}, "/Applications/Visual Studio Code.app/bin/code --new-window --wait"},
		{map[string]string{"EDITOR": "vim"}, "vim"},
	} {
		if got := strings.Join(editorCommand(env(tc.values)), " "); got != tc.want {
			t.Errorf("%v: %q, want %q", tc.values, got, tc.want)
		}
	}
	if got := tempName(resource{Filename: "../../etc/passwd"}); got != "passwd" {
		t.Errorf("tempName keeps a base name: %q", got)
	}
	if got := tempName(resource{Name: "a/b"}); got != "b" {
		t.Errorf("tempName from a name: %q", got)
	}
	if got := tempName(resource{}); got != "resource.txt" {
		t.Errorf("tempName fallback: %q", got)
	}
}

// harness is a real server over a test database, and a command line pointed
// at it with its own configuration directory.
type harness struct {
	t      *testing.T
	db     *store.Store
	user   store.User
	server *httptest.Server
	config string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	// Edits that cannot be saved are kept in their temporary directory, on
	// purpose; under the test's own, they go when it does.
	t.Setenv("TMPDIR", t.TempDir())
	ctx := context.Background()
	db, err := store.Open(ctx, testDatabaseURL(t), bytes.Repeat([]byte{7}, 32), newMemoryBlobs())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	user, err := db.UpsertUser(ctx, "100", "alice", "Alice", "")
	if err != nil {
		t.Fatal(err)
	}
	var handler http.Handler
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	t.Cleanup(server.Close)
	app := web.New(web.Config{
		PublicURL: server.URL, MaxContent: 4 << 20, SessionTTL: time.Hour,
		AllowedIDs: map[string]bool{"100": true}, RegistrationMode: auth.RegistrationAllowlist, Version: "test",
		AnonymousEnabled: true,
	}, db, upstream.New(4<<20), nil)
	handler = app.Handler()
	return &harness{t: t, db: db, user: user, server: server, config: t.TempDir()}
}

// run runs one command line invocation and returns what it printed.
func (h *harness) run(c *cli, args ...string) (int, string, string) {
	h.t.Helper()
	var stdout, stderr bytes.Buffer
	if c == nil {
		c = &cli{}
	}
	c.stdout, c.stderr = &stdout, &stderr
	if c.stdin == nil {
		c.stdin = strings.NewReader("")
	}
	env := map[string]string{"PLAINMOTE_CONFIG_DIR": h.config, "PLAINMOTE_SERVER": h.server.URL, "LANG": "en_US.UTF-8"}
	c.getenv = func(name string) string { return env[name] }
	if c.openURL == nil {
		c.openURL = func(string) bool { return false }
	}
	if c.runEditor == nil {
		c.runEditor = func([]string) error { return nil }
	}
	code := c.run(context.Background(), args)
	return code, stdout.String(), stderr.String()
}

// signIn saves a token for the harness's server the way login would.
func (h *harness) signIn(scope string) {
	h.t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	deviceCode, userCode, grant, err := h.db.CreateDeviceGrant(ctx, store.DeviceRequest{Scope: scope}, now)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.db.DecideDeviceGrant(ctx, h.user.ID, grant.ID, userCode, true, now); err != nil {
		h.t.Fatal(err)
	}
	exchange, err := h.db.ExchangeDeviceCode(ctx, deviceCode, now)
	if err != nil {
		h.t.Fatal(err)
	}
	creds := credentials{Server: h.server.URL, Servers: map[string]account{h.server.URL: {Token: exchange.Token, Login: "alice", Scope: scope}}}
	if err := creds.save(filepath.Join(h.config, "credentials")); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) resource(name, filename, body string) store.Resource {
	h.t.Helper()
	r, err := h.db.CreateResource(context.Background(), h.user.ID, name, filename, []byte(body), "", "")
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}

func (h *harness) content(id string) (string, int) {
	h.t.Helper()
	r, err := h.db.ResourceForOwner(context.Background(), h.user.ID, id)
	if err != nil {
		h.t.Fatal(err)
	}
	body, err := h.db.ReadContent(context.Background(), r)
	if err != nil {
		h.t.Fatal(err)
	}
	return string(body), r.Version
}

// TestLoginAndLogout signs in through the device flow, approving the code
// the command line prints, and signs out again.
func TestLoginAndLogout(t *testing.T) {
	h := newHarness(t)
	var stderr lockedBuffer
	c := &cli{}
	done := make(chan int)
	var stdout bytes.Buffer
	go func() {
		c.stdout, c.stderr, c.stdin = &stdout, &stderr, strings.NewReader("")
		env := map[string]string{"PLAINMOTE_CONFIG_DIR": h.config, "PLAINMOTE_SERVER": h.server.URL, "LANG": "en"}
		c.getenv = func(name string) string { return env[name] }
		c.openURL = func(address string) bool {
			if strings.Contains(address, "code") || !strings.HasSuffix(address, "/cli/device") {
				t.Errorf("the browser was sent to %q", address)
			}
			return true
		}
		done <- c.run(context.Background(), []string{"login"})
	}()
	var code string
	for deadline := time.Now().Add(5 * time.Second); code == "" && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if match := regexp.MustCompile(`Code   ([A-Z2-9]{4}-[A-Z2-9]{4})`).FindStringSubmatch(stderr.String()); match != nil {
			code = match[1]
		}
	}
	if code == "" {
		t.Fatalf("no code printed: %s", stderr.String())
	}
	normalized, _ := store.NormalizeUserCode(code)
	grant, err := h.db.PendingDeviceGrant(context.Background(), normalized, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if grant.Scope != "write" || !strings.Contains(grant.DeviceOS, "/") {
		t.Fatalf("the request: %+v", grant)
	}
	if err := h.db.DecideDeviceGrant(context.Background(), h.user.ID, grant.ID, normalized, true, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	select {
	case exit := <-done:
		if exit != 0 || !strings.Contains(stdout.String(), "Signed in as alice (read & write") {
			t.Fatalf("login: %d %s %s", exit, stdout.String(), stderr.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatal("login did not finish")
	}
	info, err := os.Stat(filepath.Join(h.config, "credentials"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the credentials file: %v %v", info, err)
	}
	if exit, out, _ := h.run(nil, "whoami"); exit != 0 || !strings.Contains(out, "alice on "+h.server.URL) {
		t.Fatalf("whoami: %d %s", exit, out)
	}
	saved, _ := loadCredentials(filepath.Join(h.config, "credentials"))
	token := saved.Servers[h.server.URL].Token
	if exit, out, _ := h.run(nil, "logout"); exit != 0 || !strings.Contains(out, "revoked") {
		t.Fatalf("logout: %d %s", exit, out)
	}
	if _, _, err := h.db.TokenUser(context.Background(), token, "", time.Now().UTC()); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("logout left the token working")
	}
	if exit, _, errOut := h.run(nil, "ls"); exit != 1 || !strings.Contains(errOut, "not signed in") {
		t.Fatalf("after logout: %d %s", exit, errOut)
	}
}

func TestListCatPush(t *testing.T) {
	h := newHarness(t)
	h.signIn("write")
	conf := h.resource("nginx", "nginx.conf", "listen 80;\n")
	h.resource("app", "app.log", "boot\n")

	if exit, out, _ := h.run(nil, "ls"); exit != 0 || !strings.Contains(out, "nginx.conf") || !strings.Contains(out, "app.log") || !strings.Contains(out, conf.ID[:8]) {
		t.Fatalf("ls: %d %s", exit, out)
	}
	if exit, out, _ := h.run(nil, "ls", "nginx"); exit != 0 || strings.Contains(out, "app.log") {
		t.Fatalf("ls with a keyword: %d %s", exit, out)
	}
	if exit, out, _ := h.run(nil, "cat", "nginx.conf"); exit != 0 || out != "listen 80;\n" {
		t.Fatalf("cat: %d %q", exit, out)
	}
	if exit, _, errOut := h.run(nil, "cat", "nothing-like-this"); exit != 1 || !strings.Contains(errOut, "no resource matches") {
		t.Fatalf("cat of nothing: %d %s", exit, errOut)
	}

	file := filepath.Join(t.TempDir(), "nginx.conf")
	if err := os.WriteFile(file, []byte("listen 8080;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	host := strings.TrimPrefix(h.server.URL, "http://")
	if exit, out, _ := h.run(nil, "push", file, "--to", conf.ID[:8]); exit != 0 || !strings.Contains(out, "Saved as v2 on "+host) {
		t.Fatalf("push --to: %d %s", exit, out)
	}
	if body, version := h.content(conf.ID); body != "listen 8080;\n" || version != 2 {
		t.Fatalf("after push: %q v%d", body, version)
	}
	if exit, out, _ := h.run(nil, "push", file, "--to", "nginx.conf"); exit != 0 || !strings.Contains(out, "already v2") {
		t.Fatalf("pushing the same content: %d %s", exit, out)
	}
	if exit, out, _ := h.run(&cli{stdin: strings.NewReader("line\n")}, "push", "-", "--name", "from stdin"); exit != 0 || !strings.Contains(out, "Created from stdin") {
		t.Fatalf("push from stdin: %d %s", exit, out)
	}
	if exit, _, errOut := h.run(&cli{stdin: strings.NewReader("x")}, "push", "-"); exit != 1 || !strings.Contains(errOut, "--name") {
		t.Fatalf("push from stdin without a name: %d %s", exit, errOut)
	}
	// Two resources called the same: the reference is ambiguous, not guessed.
	h.resource("twin", "nginx.conf", "other\n")
	if exit, _, errOut := h.run(nil, "cat", "nginx.conf"); exit != 1 || !strings.Contains(errOut, "more than one") {
		t.Fatalf("an ambiguous reference: %d %s", exit, errOut)
	}
}

func TestEdit(t *testing.T) {
	h := newHarness(t)
	h.signIn("write")
	conf := h.resource("nginx", "nginx.conf", "worker_processes 2;\nlisten 80;\n")

	var opened []string
	editor := func(change func(path string)) *cli {
		return &cli{runEditor: func(command []string) error {
			path := command[len(command)-1]
			if strings.HasSuffix(path, ".mine") {
				path = command[len(command)-2]
			}
			opened = append(opened, path)
			info, err := os.Stat(filepath.Dir(path))
			if err != nil || info.Mode().Perm() != 0o700 {
				t.Errorf("the temporary directory is not private: %v %v", info, err)
			}
			change(path)
			return nil
		}}
	}
	appendLine := func(line string) func(string) {
		return func(path string) {
			body, _ := os.ReadFile(path)
			_ = os.WriteFile(path, append(body, []byte(line)...), 0o600)
		}
	}

	if exit, out, _ := h.run(editor(func(string) {}), "edit", "nginx.conf"); exit != 0 || !strings.Contains(out, "No changes") {
		t.Fatalf("an unchanged edit: %d %s", exit, out)
	}
	exit, out, errOut := h.run(editor(appendLine("server_name a;\n")), "edit", "nginx.conf")
	if host := strings.TrimPrefix(h.server.URL, "http://"); exit != 0 || !strings.Contains(out, "Saved as v2 on "+host) || !strings.Contains(errOut, "from "+host) {
		t.Fatalf("an edit: %d %s", exit, out)
	}
	if body, version := h.content(conf.ID); version != 2 || !strings.HasSuffix(body, "server_name a;\n") {
		t.Fatalf("after the edit: %q v%d", body, version)
	}
	if filepath.Base(opened[len(opened)-1]) != "nginx.conf" {
		t.Fatalf("the editor opened %q", opened[len(opened)-1])
	}
	if _, err := os.Stat(filepath.Dir(opened[len(opened)-1])); !os.IsNotExist(err) {
		t.Fatal("the temporary directory was left behind")
	}

	// Saved elsewhere while the editor was open, nobody at the terminal: the
	// edit is kept, not lost and not forced.
	elsewhere := func(path string) {
		if err := h.db.UpdateResource(context.Background(), h.user.ID, conf.ID, "nginx", "nginx.conf", []byte("worker_processes 4;\n"), "utf-8", ""); err != nil {
			t.Fatal(err)
		}
		appendLine("mine;\n")(path)
	}
	exit, _, errOut = h.run(editor(elsewhere), "edit", "nginx.conf")
	kept := regexp.MustCompile(`kept in (\S+)`).FindStringSubmatch(errOut)
	if exit != 1 || !strings.Contains(errOut, "saved elsewhere as v3") || kept == nil {
		t.Fatalf("a conflict without a terminal: %d %s", exit, errOut)
	}
	if body, err := os.ReadFile(kept[1]); err != nil || !strings.Contains(string(body), "mine;") {
		t.Fatalf("the kept file: %q %v", body, err)
	}
	if !strings.Contains(errOut, "- worker_processes 4;") || !strings.Contains(errOut, "+ mine;") {
		t.Fatalf("the conflict shows the difference: %s", errOut)
	}
	_ = os.RemoveAll(filepath.Dir(kept[1]))

	// With someone there: save over.
	c := editor(func(path string) {
		_ = h.db.UpdateResource(context.Background(), h.user.ID, conf.ID, "nginx", "nginx.conf", []byte("worker_processes 8;\n"), "utf-8", "")
		appendLine("forced;\n")(path)
	})
	c.interactive, c.stdin = true, strings.NewReader("o\n")
	if exit, out, errOut := h.run(c, "edit", "nginx.conf"); exit != 0 || !strings.Contains(out, "Saved as v5") {
		t.Fatalf("save over: %d %s %s", exit, out, errOut)
	}
	if body, _ := h.content(conf.ID); !strings.HasSuffix(body, "forced;\n") {
		t.Fatalf("after saving over: %q", body)
	}

	// Or edit again on top of what is current, with the earlier edit beside it.
	round := 0
	c = &cli{interactive: true, stdin: strings.NewReader("r\n"), runEditor: func(command []string) error {
		round++
		path := command[len(command)-1]
		if round == 1 {
			_ = h.db.UpdateResource(context.Background(), h.user.ID, conf.ID, "nginx", "nginx.conf", []byte("elsewhere;\n"), "utf-8", "")
			appendLine("first try;\n")(path)
			return nil
		}
		if !strings.HasSuffix(path, ".mine") {
			t.Errorf("the second round does not show the earlier edit: %v", command)
		}
		mine, _ := os.ReadFile(path)
		current := command[len(command)-2]
		body, _ := os.ReadFile(current)
		if string(body) != "elsewhere;\n" || !strings.Contains(string(mine), "first try;") {
			t.Errorf("the second round opened %q beside %q", body, mine)
		}
		return os.WriteFile(current, []byte("elsewhere;\nmerged;\n"), 0o600)
	}}
	if exit, out, errOut := h.run(c, "edit", "nginx.conf"); exit != 0 || !strings.Contains(out, "Saved as v7") {
		t.Fatalf("edit again: %d %s %s", exit, out, errOut)
	}
	if body, _ := h.content(conf.ID); body != "elsewhere;\nmerged;\n" {
		t.Fatalf("after editing again: %q", body)
	}

	// A read-only sign-in edits nothing, and says so.
	h.signIn("read")
	if exit, _, errOut := h.run(editor(appendLine("x\n")), "edit", "nginx.conf"); exit != 1 || !strings.Contains(errOut, "read-only") {
		t.Fatalf("a read-only edit: %d %s", exit, errOut)
	}
}

// share prints the link alone on stdout, so it can be captured, and the
// quick share it made is listed as one and refused by edit.
func TestShare(t *testing.T) {
	h := newHarness(t)
	h.signIn("write")
	exit, out, errOut := h.run(&cli{stdin: strings.NewReader("boot ok\n")}, "share", "-", "--filename", "app.log", "--ttl", "1d")
	if exit != 0 || !strings.HasPrefix(out, h.server.URL+"/d/") || !strings.HasSuffix(out, "/app.log\n") || strings.Count(out, "\n") != 1 ||
		!strings.Contains(errOut, "Shared on "+strings.TrimPrefix(h.server.URL, "http://")) {
		t.Fatalf("share: %d %q %q", exit, out, errOut)
	}
	response, err := http.Get(strings.TrimSpace(out))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if string(body) != "boot ok\n" {
		t.Fatalf("the link delivers %q", body)
	}
	if exit, out, _ := h.run(nil, "ls"); exit != 0 || !strings.Contains(out, "quick share") {
		t.Fatalf("ls: %d %s", exit, out)
	}
	if exit, out, _ := h.run(nil, "cat", "app.log"); exit != 0 || out != "boot ok\n" {
		t.Fatalf("cat: %d %q", exit, out)
	}
	if exit, _, errOut := h.run(nil, "edit", "app.log"); exit != 1 || !strings.Contains(errOut, "quick share and read only") {
		t.Fatalf("edit: %d %s", exit, errOut)
	}
	if exit, _, errOut := h.run(&cli{stdin: strings.NewReader("x")}, "share", "-", "--ttl", "2h"); exit != 1 || !strings.Contains(errOut, "10m, 1h, 1d, 7d or 30d") {
		t.Fatalf("an unknown lifetime: %d %s", exit, errOut)
	}
	h.signIn("read")
	if exit, _, errOut := h.run(&cli{stdin: strings.NewReader("x")}, "share", "-"); exit != 1 || errOut == "" {
		t.Fatalf("share with a read-only sign-in: %d %s", exit, errOut)
	}
}

func TestEditRefusesWhatIsNotText(t *testing.T) {
	h := newHarness(t)
	h.signIn("write")
	h.resource("logo", "logo.png", "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	if _, err := h.db.CreateResource(context.Background(), h.user.ID, "remote", "", nil, "", "https://example.com/x"); err != nil {
		t.Fatal(err)
	}
	if exit, _, errOut := h.run(nil, "edit", "logo.png"); exit != 1 || !strings.Contains(errOut, "not text") {
		t.Fatalf("editing a picture: %d %s", exit, errOut)
	}
	if exit, _, errOut := h.run(nil, "edit", "remote"); exit != 1 || !strings.Contains(errOut, "remote address") {
		t.Fatalf("editing a reference: %d %s", exit, errOut)
	}
}

// lockedBuffer is a buffer two goroutines can share.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func testDatabaseURL(t *testing.T) string { return testsupport.DatabaseURL(t) }

func newMemoryBlobs() *testsupport.MemoryBlobs { return testsupport.NewMemoryBlobs() }

// TestLoginOpensTheBrowserOnEnter: the code comes first, and the browser
// opens only when Enter is pressed - and never with the code in its address.
func TestLoginOpensTheBrowserOnEnter(t *testing.T) {
	h := newHarness(t)
	keys, typing := io.Pipe()
	var stderr lockedBuffer
	var opened []string
	var mu sync.Mutex
	c := &cli{interactive: true, stdin: keys, stderr: &stderr, stdout: io.Discard,
		openURL: func(address string) bool {
			mu.Lock()
			defer mu.Unlock()
			opened = append(opened, address)
			return true
		}}
	env := map[string]string{"PLAINMOTE_CONFIG_DIR": h.config, "PLAINMOTE_SERVER": h.server.URL, "LANG": "en"}
	c.getenv = func(name string) string { return env[name] }
	done := make(chan int)
	go func() { done <- c.run(context.Background(), []string{"login"}) }()

	var code string
	for deadline := time.Now().Add(5 * time.Second); code == "" && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if match := regexp.MustCompile(`Code   ([A-Z2-9]{4}-[A-Z2-9]{4})`).FindStringSubmatch(stderr.String()); match != nil {
			code = match[1]
		}
	}
	if code == "" || !strings.Contains(stderr.String(), "Press Enter") {
		t.Fatalf("the code and the prompt come first: %s", stderr.String())
	}
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	early := len(opened)
	mu.Unlock()
	if early != 0 {
		t.Fatal("the browser opened before Enter")
	}
	_, _ = typing.Write([]byte("\n"))
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		mu.Lock()
		n := len(opened)
		mu.Unlock()
		if n > 0 {
			break
		}
	}
	mu.Lock()
	if len(opened) != 1 || strings.Contains(opened[0], code) || !strings.HasSuffix(opened[0], "/cli/device") {
		t.Fatalf("Enter opened %v", opened)
	}
	mu.Unlock()

	normalized, _ := store.NormalizeUserCode(code)
	grant, err := h.db.PendingDeviceGrant(context.Background(), normalized, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.db.DecideDeviceGrant(context.Background(), h.user.ID, grant.ID, normalized, true, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	select {
	case exit := <-done:
		if exit != 0 {
			t.Fatalf("login: %d %s", exit, stderr.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatal("login did not finish")
	}
	_ = typing.Close()
}

func TestConfigLanguageAndEditor(t *testing.T) {
	h := newHarness(t)
	if exit, out, _ := h.run(nil, "config", "language", "zh"); exit != 0 || !strings.Contains(out, "已保存") {
		t.Fatalf("config language: %d %s", exit, out)
	}
	// The saved language wins over the locale, and PLAINMOTE_LANG over both.
	if _, _, errOut := h.run(nil, "ls"); !strings.Contains(errOut, "尚未登录") {
		t.Fatalf("the saved language is used: %s", errOut)
	}
	c := &cli{getenv: func(name string) string {
		return map[string]string{"PLAINMOTE_CONFIG_DIR": h.config, "PLAINMOTE_LANG": "en", "LANG": "zh_CN.UTF-8"}[name]
	}}
	if c.language() != 0 {
		t.Fatal("PLAINMOTE_LANG wins over the saved language")
	}
	if exit, _, _ := h.run(nil, "config", "language", "klingon"); exit != 2 {
		t.Fatal("an unknown language is refused")
	}
	if exit, _, _ := h.run(nil, "config", "language", "auto"); exit != 0 {
		t.Fatal("auto clears the language")
	}
	if exit, _, _ := h.run(nil, "config", "editor", "zed", "--new-window"); exit != 0 {
		t.Fatal("config editor")
	}
	if _, out, _ := h.run(nil, "config"); !strings.Contains(out, "editor    zed --new-window") || !strings.Contains(out, "language  auto") {
		t.Fatalf("config shows the settings: %s", out)
	}
}

// TestEditorIsChosenOnce: with no editor set anywhere, the first edit asks
// which of the editors found here to use, remembers it, and does not ask
// again.
func TestEditorIsChosenOnce(t *testing.T) {
	h := newHarness(t)
	h.signIn("write")
	h.resource("nginx", "nginx.conf", "listen 80;\n")
	var used [][]string
	newCLI := func(answer string) *cli {
		return &cli{interactive: true, stdin: strings.NewReader(answer),
			findEditors: func() []editorChoice {
				return []editorChoice{{Name: "Zed", Command: "zed --wait"}, {Name: "Vim", Command: "vim"}}
			},
			runEditor: func(command []string) error {
				used = append(used, command)
				return nil
			}}
	}
	exit, _, errOut := h.run(newCLI("2\n"), "edit", "nginx.conf")
	if exit != 0 || !strings.Contains(errOut, "1) Zed") || !strings.Contains(errOut, "Using Vim from now on") || used[0][0] != "vim" {
		t.Fatalf("the first edit asks: %d %s %v", exit, errOut, used)
	}
	exit, _, errOut = h.run(newCLI(""), "edit", "nginx.conf")
	if exit != 0 || strings.Contains(errOut, "1) Zed") || used[1][0] != "vim" {
		t.Fatalf("the choice is remembered: %d %s %v", exit, errOut, used)
	}
	saved, _ := loadCredentials(filepath.Join(h.config, "credentials"))
	if saved.Editor != "vim" {
		t.Fatalf("saved editor %q", saved.Editor)
	}
}

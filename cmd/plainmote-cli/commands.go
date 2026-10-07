package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"
)

// login signs this device in by device authorization: the server hands out
// a code, the person enters it in a signed-in browser and approves, and the
// token comes back here. The code is shown, never put in the address opened:
// typing it is what makes sure the person approving is the one who saw it.
func (c *cli) login(ctx context.Context, args []string) error {
	var readOnly, noBrowser bool
	args, server, err := c.flags("login", args, func(set *flag.FlagSet) {
		set.BoolVar(&readOnly, "read-only", false, "sign in with read-only access")
		set.BoolVar(&noBrowser, "no-browser", false, "print the address without opening a browser")
	})
	if err != nil {
		return err
	}
	if err := c.needArgs(args, 0, "login", "--read-only"); err != nil {
		return err
	}
	s, err := c.session(server)
	if err != nil {
		return err
	}
	// Signing in again replaces the old sign-in rather than leaving it live.
	if old := s.creds.Servers[s.server].Token; old != "" {
		_ = newClient(s.server, old).revoke(ctx)
	}
	scope := "write"
	if readOnly {
		scope = "read"
	}
	host, _ := os.Hostname()
	api := newClient(s.server, "")
	var start struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
	}
	if err := api.postJSON(ctx, "/api/v1/device/code", map[string]string{
		"scope": scope, "device": host, "os": runtime.GOOS + "/" + runtime.GOARCH, "version": version,
	}, &start); err != nil {
		return err
	}
	// The code is shown first, and the browser opens only when asked: the
	// person reads the code before anything takes them away from it. The
	// sign-in is polled meanwhile, so the address can as well be opened by
	// hand, on another device.
	fmt.Fprintln(c.stderr, msg("login_open"))
	fmt.Fprintf(c.stderr, "\n    %s\n    %s   %s\n\n", start.VerificationURI, msg("login_code"), start.UserCode)
	fmt.Fprintln(c.stderr, msg("login_valid", max(start.ExpiresIn/60, 1)))
	var enter <-chan struct{}
	if !noBrowser && c.getenv("PLAINMOTE_NO_BROWSER") == "" && c.interactive {
		fmt.Fprintln(c.stderr, msg("login_press_enter"))
		pressed := make(chan struct{})
		go func() {
			if _, err := bufio.NewReader(c.stdin).ReadString('\n'); err == nil {
				close(pressed)
			}
		}()
		enter = pressed
	} else {
		fmt.Fprintln(c.stderr, msg("login_waiting"))
	}

	interval := max(start.Interval, 1)
	poll := time.NewTimer(time.Duration(interval) * time.Second)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-enter:
			enter = nil
			if c.openURL(start.VerificationURI) {
				fmt.Fprintln(c.stderr, msg("login_opened"))
			} else {
				fmt.Fprintln(c.stderr, msg("login_open_failed"))
			}
			continue
		case <-poll.C:
		}
		poll.Reset(time.Duration(interval) * time.Second)
		var issued struct {
			AccessToken string    `json:"access_token"`
			Scope       string    `json:"scope"`
			ExpiresAt   time.Time `json:"expires_at"`
			Login       string    `json:"login"`
		}
		err := api.postJSON(ctx, "/api/v1/device/token", map[string]string{"device_code": start.DeviceCode}, &issued)
		var refusal *apiError
		if errors.As(err, &refusal) {
			switch refusal.Code {
			case "authorization_pending":
				continue
			case "slow_down":
				interval = max(refusal.Interval, interval+5)
				continue
			case "access_denied":
				return fmt.Errorf("%s", msg("login_denied"))
			case "expired_token":
				return fmt.Errorf("%s", msg("login_expired"))
			}
		}
		if err != nil {
			return err
		}
		if s.creds.Servers == nil {
			s.creds.Servers = map[string]account{}
		}
		s.creds.Servers[s.server] = account{Token: issued.AccessToken, Login: issued.Login, Scope: issued.Scope, ExpiresAt: issued.ExpiresAt}
		if s.creds.Server == "" {
			s.creds.Server = s.server
		}
		if err := s.creds.save(s.path); err != nil {
			return err
		}
		fmt.Fprintln(c.stdout, msg("login_done", issued.Login, scopeText(issued.Scope), issued.ExpiresAt.Local().Format("2006-01-02")))
		fmt.Fprintln(c.stderr, msg("login_saved", s.path))
		c.noteVersion(api, s.server)
		return nil
	}
}

func scopeText(scope string) string {
	if scope == "write" {
		return msg("scope_write")
	}
	return msg("scope_read")
}

// noteVersion mentions a command line older or newer than its server.
func (c *cli) noteVersion(api *client, server string) {
	if version != "dev" && api.serverVersion != "" && api.serverVersion != "dev" && api.serverVersion != version {
		fmt.Fprintln(c.stderr, msg("version_mismatch", api.serverVersion, version, server))
	}
}

func (c *cli) logout(ctx context.Context, args []string) error {
	args, server, err := c.flags("logout", args, nil)
	if err != nil {
		return err
	}
	if err := c.needArgs(args, 0, "logout", "nothing"); err != nil {
		return err
	}
	s, err := c.session(server)
	if err != nil {
		return err
	}
	saved, ok := s.creds.Servers[s.server]
	if !ok {
		fmt.Fprintln(c.stdout, msg("logout_none", s.server))
		return nil
	}
	if err := newClient(s.server, saved.Token).revoke(ctx); err != nil {
		return err
	}
	delete(s.creds.Servers, s.server)
	if err := s.creds.save(s.path); err != nil {
		return err
	}
	fmt.Fprintln(c.stdout, msg("logout_done"))
	return nil
}

func (c *cli) whoamiCommand(ctx context.Context, args []string) error {
	args, server, err := c.flags("whoami", args, nil)
	if err != nil {
		return err
	}
	if err := c.needArgs(args, 0, "whoami", "nothing"); err != nil {
		return err
	}
	s, api, err := c.signedIn(server)
	if err != nil {
		return err
	}
	info, err := api.whoami(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintln(c.stdout, msg("whoami", info.Login, s.server, scopeText(info.Scope), info.ExpiresAt.Local().Format("2006-01-02")))
	c.noteVersion(api, s.server)
	return nil
}

// serverCommand shows or sets the server. The installer sets it with
// --if-unset, so installing from a second server does not move a working
// setup.
func (c *cli) serverCommand(args []string) error {
	var ifUnset bool
	args, _, err := c.flags("server", args, func(set *flag.FlagSet) {
		set.BoolVar(&ifUnset, "if-unset", false, "only if no server is set yet")
	})
	if err != nil {
		return err
	}
	s, err := c.session("")
	if err != nil && len(args) == 0 {
		return err
	}
	if len(args) == 0 {
		fmt.Fprintln(c.stdout, msg("server_is", s.server))
		return nil
	}
	if len(args) != 1 {
		return c.needArgs(args, 1, "server", "<url>")
	}
	server, err := normalizeServer(args[0])
	if err != nil {
		return err
	}
	path, err := c.credentialsPath()
	if err != nil {
		return err
	}
	creds, err := loadCredentials(path)
	if err != nil {
		return err
	}
	if ifUnset && creds.Server != "" {
		return nil
	}
	creds.Server = server
	if err := creds.save(path); err != nil {
		return err
	}
	fmt.Fprintln(c.stdout, msg("server_set", server))
	return nil
}

func (c *cli) ls(ctx context.Context, args []string) error {
	var decrypt bool
	args, server, err := c.flags("ls", args, func(set *flag.FlagSet) {
		set.BoolVar(&decrypt, "decrypt", false, "show encrypted resources' names, asking for the master password")
	})
	if err != nil {
		return err
	}
	_, api, err := c.signedIn(server)
	if err != nil {
		return err
	}
	keyword := strings.Join(args, " ")
	// The service cannot match names it cannot read: decrypting, the whole
	// list is fetched and the keyword matched here, encrypted names included.
	query := keyword
	if decrypt {
		query = ""
	}
	resources, err := api.list(ctx, query)
	if err != nil {
		return err
	}
	if decrypt {
		var kept []resource
		for _, r := range resources {
			if r.isSealed() {
				opened, err := c.openSealed(ctx, api, r)
				if err != nil {
					return err
				}
				r.Name, r.Filename = opened.meta.Name, opened.meta.Filename
			}
			lower := strings.ToLower(keyword)
			if keyword == "" || strings.Contains(strings.ToLower(r.Name), lower) || strings.Contains(strings.ToLower(r.Filename), lower) {
				kept = append(kept, r)
			}
		}
		resources = kept
	}
	if len(resources) == 0 {
		fmt.Fprintln(c.stdout, msg("ls_empty"))
		return nil
	}
	table := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, msg("ls_header"))
	for _, r := range resources {
		name := r.Name
		switch {
		case name == "" && r.isSealed() && r.Filename == "":
			name = msg("encrypted_name")
		case name == "":
			name = msg("untitled")
		}
		size := sizeText(r.Size)
		if r.Remote {
			size = msg("remote_mark")
		}
		version := fmt.Sprintf("v%d", r.Version)
		if r.ExpiresAt != nil {
			version = msg("quick_mark")
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\n", clean(name), clean(r.Filename), size, version,
			r.UpdatedAt.Local().Format("2006-01-02 15:04"), r.ID[:8])
	}
	return table.Flush()
}

// clean keeps a name from moving the table around or writing to the
// terminal: control characters become spaces.
func clean(value string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, value)
}

func sizeText(size int64) string {
	switch {
	case size >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(size)/(1<<20))
	case size >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(size)/(1<<10))
	}
	return fmt.Sprintf("%d B", size)
}

func (c *cli) cat(ctx context.Context, args []string) error {
	var force bool
	args, server, err := c.flags("cat", args, func(set *flag.FlagSet) {
		set.BoolVar(&force, "force", false, "write content that is not text to a terminal")
	})
	if err != nil {
		return err
	}
	if err := c.needArgs(args, 1, "cat", "<resource>"); err != nil {
		return err
	}
	_, api, err := c.signedIn(server)
	if err != nil {
		return err
	}
	target, err := api.resolve(ctx, args[0])
	if err != nil {
		return err
	}
	if target.Remote {
		return fmt.Errorf("%s", msg("is_reference", target.label()))
	}
	terminal := false
	if file, ok := c.stdout.(*os.File); ok && isTerminal(file) {
		terminal = true
	}
	if target.isSealed() {
		opened, err := c.openSealed(ctx, api, target)
		if err != nil {
			return err
		}
		if terminal && !textLike(opened.meta.Type) && !force {
			return fmt.Errorf("%s", msg("cat_binary", sealedLabel(opened.meta, target.ID), args[0]))
		}
		body, err := opened.read(ctx, api)
		if err != nil {
			return err
		}
		_, err = c.stdout.Write(body.Body)
		return err
	}
	if terminal && !target.Editable && !force {
		return fmt.Errorf("%s", msg("cat_binary", target.label(), args[0]))
	}
	body, err := api.read(ctx, target.ID)
	if err != nil {
		return err
	}
	_, err = c.stdout.Write(body.Body)
	return err
}

func (c *cli) push(ctx context.Context, args []string) error {
	var to, name, filename string
	var encrypt bool
	args, server, err := c.flags("push", args, func(set *flag.FlagSet) {
		set.StringVar(&to, "to", "", "save as a new version of this resource")
		set.StringVar(&name, "name", "", "name of a new resource")
		set.StringVar(&filename, "filename", "", "filename of a new resource")
		set.BoolVar(&encrypt, "encrypt", false, "make the new resource end-to-end encrypted")
	})
	if err != nil {
		return err
	}
	if err := c.needArgs(args, 1, "push", "<file|->"); err != nil {
		return err
	}
	var body []byte
	if args[0] == "-" {
		if to == "" && name == "" && filename == "" {
			return fmt.Errorf("%s", msg("push_needs_name"))
		}
		body, err = io.ReadAll(c.stdin)
	} else {
		body, err = os.ReadFile(args[0])
		if filename == "" {
			filename = filepath.Base(args[0])
		}
	}
	if err != nil {
		return err
	}
	_, api, err := c.signedIn(server)
	if err != nil {
		return err
	}
	if to != "" {
		target, err := api.resolve(ctx, to)
		if err != nil {
			return err
		}
		var result saveResult
		if target.isSealed() {
			// Encrypted here, under the resource's own key, with the name
			// and filename it has.
			opened, err := c.openSealed(ctx, api, target)
			if err != nil {
				return err
			}
			if !textLike(opened.meta.Type) || !textLike(guessType(opened.meta.Filename, body)) {
				opened.meta.Type = guessType(opened.meta.Filename, body)
			}
			result, err = opened.write(ctx, api, body, 0)
			if err != nil {
				return err
			}
		} else if result, err = api.write(ctx, target.ID, body, 0, ""); err != nil {
			return err
		}
		if result.NewVersion {
			fmt.Fprintln(c.stdout, msg("push_saved", result.Version, api.host()))
		} else {
			fmt.Fprintln(c.stdout, msg("push_same", result.Version, api.host()))
		}
		return nil
	}
	if encrypt {
		made, err := c.createSealed(ctx, api, name, filename, body)
		if err != nil {
			return err
		}
		label := name
		if label == "" {
			label = filename
		}
		fmt.Fprintln(c.stdout, msg("push_created_sealed", clean(label), api.host(), made.URL))
		return nil
	}
	made, err := api.create(ctx, name, filename, body)
	if err != nil {
		return err
	}
	fmt.Fprintln(c.stdout, msg("push_created", made.label(), api.host(), made.URL))
	return nil
}

// share makes a quick share: the link goes to stdout on its own, so it can
// be piped or captured, and what it is and how long it lasts to stderr.
func (c *cli) share(ctx context.Context, args []string) error {
	var ttl, filename string
	var encrypt bool
	args, server, err := c.flags("share", args, func(set *flag.FlagSet) {
		set.StringVar(&ttl, "ttl", "", "how long the link works: 10m, 1h, 1d, 7d or 30d")
		set.StringVar(&filename, "filename", "", "name at the end of the link")
		set.BoolVar(&encrypt, "encrypt", false, "end-to-end encrypt it under the master password")
	})
	if err != nil {
		return err
	}
	if err := c.needArgs(args, 1, "share", "<file|->"); err != nil {
		return err
	}
	var body []byte
	if args[0] == "-" {
		body, err = io.ReadAll(c.stdin)
	} else {
		body, err = os.ReadFile(args[0])
		if filename == "" {
			filename = filepath.Base(args[0])
		}
	}
	if err != nil {
		return err
	}
	_, api, err := c.signedIn(server)
	if err != nil {
		return err
	}
	var made quickShare
	address := ""
	if encrypt {
		// The key goes after #, which is never sent to the server: the
		// link opens in a browser, which decrypts it.
		var linkKey []byte
		made, linkKey, err = c.shareSealed(ctx, api, filename, ttl, body)
		address = made.ShareURL + "#k=" + base64.RawURLEncoding.EncodeToString(linkKey)
	} else {
		made, err = api.share(ctx, filename, ttl, body)
		address = made.ShareURL
	}
	if err != nil {
		return err
	}
	fmt.Fprintln(c.stdout, address)
	if made.ExpiresAt != nil {
		fmt.Fprintln(c.stderr, msg("share_created", api.host(), made.ExpiresAt.Local().Format("2006-01-02 15:04")))
	}
	return nil
}

// configCommand shows or changes the saved settings: the language messages
// are in, and the editor edit opens.
//
//	plainmote config                     show them
//	plainmote config language zh|en|auto
//	plainmote config editor <command>    or --unset
func (c *cli) configCommand(args []string) error {
	// An editor command is kept as written, flags of its own included:
	// "zed --new-window" is not plainmote's to parse.
	var editor []string
	if len(args) > 0 && args[0] == "editor" {
		editor, args = args[1:], args[:1]
	}
	var unset bool
	args, _, err := c.flags("config", args, func(set *flag.FlagSet) {
		set.BoolVar(&unset, "unset", false, "clear the setting")
	})
	if err != nil {
		return err
	}
	if len(editor) == 1 && editor[0] == "--unset" {
		editor, unset = nil, true
	}
	args = append(args, editor...)
	path, err := c.credentialsPath()
	if err != nil {
		return err
	}
	creds, err := loadCredentials(path)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		s, _ := c.session("")
		language, editor := creds.Language, creds.Editor
		if language == "" {
			language = "auto"
		}
		if editor == "" {
			editor = "-"
		}
		fmt.Fprintf(c.stdout, "server    %s\nlanguage  %s\neditor    %s\n", s.server, language, editor)
		return nil
	}
	switch args[0] {
	case "language":
		if len(args) != 2 && !unset {
			return c.needArgs(args, 2, "config language", "zh|en|auto")
		}
		value := "auto"
		if len(args) == 2 {
			value = strings.ToLower(args[1])
		}
		switch value {
		case "zh", "en":
			creds.Language = value
		case "auto":
			creds.Language = ""
		default:
			return c.needArgs(nil, 1, "config language", "zh|en|auto")
		}
	case "editor":
		switch {
		case unset:
			creds.Editor = ""
		case len(args) >= 2:
			creds.Editor = strings.Join(args[1:], " ")
		default:
			return c.needArgs(nil, 1, "config editor", "<command>")
		}
	default:
		return c.needArgs(nil, 1, "config", "language|editor")
	}
	if err := creds.save(path); err != nil {
		return err
	}
	lang = c.language()
	fmt.Fprintln(c.stdout, msg("config_saved"))
	return nil
}

// sealedLabel names an opened encrypted resource in messages.
func sealedLabel(meta sealedMeta, id string) string {
	switch {
	case meta.Name != "":
		return clean(meta.Name)
	case meta.Filename != "":
		return clean(meta.Filename)
	}
	return id[:8]
}

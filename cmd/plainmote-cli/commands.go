package main

import (
	"context"
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
	opened := !noBrowser && c.getenv("PLAINMOTE_NO_BROWSER") == "" && c.openURL(start.VerificationURI)
	if opened {
		fmt.Fprintln(c.stderr, msg("login_open"))
	} else {
		fmt.Fprintln(c.stderr, msg("login_open_any"))
	}
	fmt.Fprintf(c.stderr, "\n    %s\n    %s   %s\n\n", start.VerificationURI, msg("login_code"), start.UserCode)
	minutes := max(start.ExpiresIn/60, 1)
	if opened {
		fmt.Fprintln(c.stderr, msg("login_browser", minutes))
	} else {
		fmt.Fprintln(c.stderr, msg("login_valid", minutes))
	}
	fmt.Fprint(c.stderr, msg("login_waiting"))

	interval := max(start.Interval, 1)
	for {
		select {
		case <-ctx.Done():
			fmt.Fprintln(c.stderr)
			return ctx.Err()
		case <-time.After(time.Duration(interval) * time.Second):
		}
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
				fmt.Fprintln(c.stderr)
				return fmt.Errorf("%s", msg("login_denied"))
			case "expired_token":
				fmt.Fprintln(c.stderr)
				return fmt.Errorf("%s", msg("login_expired"))
			}
		}
		if err != nil {
			fmt.Fprintln(c.stderr)
			return err
		}
		fmt.Fprintln(c.stderr, " ✓")
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
	args, server, err := c.flags("ls", args, nil)
	if err != nil {
		return err
	}
	_, api, err := c.signedIn(server)
	if err != nil {
		return err
	}
	resources, err := api.list(ctx, strings.Join(args, " "))
	if err != nil {
		return err
	}
	if len(resources) == 0 {
		fmt.Fprintln(c.stdout, msg("ls_empty"))
		return nil
	}
	table := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, msg("ls_header"))
	for _, r := range resources {
		name := r.Name
		if name == "" {
			name = msg("untitled")
		}
		size := sizeText(r.Size)
		if r.Remote {
			size = msg("remote_mark")
		}
		fmt.Fprintf(table, "%s\t%s\t%s\tv%d\t%s\t%s\n", clean(name), clean(r.Filename), size, r.Version,
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
	if file, ok := c.stdout.(*os.File); ok && isTerminal(file) && !target.Editable && !force {
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
	args, server, err := c.flags("push", args, func(set *flag.FlagSet) {
		set.StringVar(&to, "to", "", "save as a new version of this resource")
		set.StringVar(&name, "name", "", "name of a new resource")
		set.StringVar(&filename, "filename", "", "filename of a new resource")
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
		result, err := api.write(ctx, target.ID, body, 0, "")
		if err != nil {
			return err
		}
		if result.NewVersion {
			fmt.Fprintln(c.stdout, msg("push_saved", result.Version, api.host()))
		} else {
			fmt.Fprintln(c.stdout, msg("push_same", result.Version, api.host()))
		}
		return nil
	}
	made, err := api.create(ctx, name, filename, body)
	if err != nil {
		return err
	}
	fmt.Fprintln(c.stdout, msg("push_created", made.label(), api.host(), made.URL))
	return nil
}

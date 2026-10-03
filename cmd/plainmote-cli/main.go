// Command plainmote is the PlainMote command line: sign a device in, list,
// read, edit and upload resources. It talks to a server's /api/v1 with a
// personal access token obtained by device sign-in, and keeps nothing but
// that token and the server's address.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
)

// version is set at build time to the server revision it was built with.
var version = "dev"

// cli is one run of the command line, with everything it touches outside
// itself passed in, so tests can run it whole.
type cli struct {
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	getenv func(string) string
	// interactive reports whether a person is at the terminal to answer.
	interactive bool
	// openURL tries to show an address in a browser.
	openURL func(string) bool
	// runEditor runs the editor command on files and waits for it.
	runEditor func(command []string) error
	// findEditors lists the editors installed here, to choose from.
	findEditors func() []editorChoice
}

func main() {
	app := &cli{
		stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr, getenv: os.Getenv,
		interactive: isTerminal(os.Stdin) && isTerminal(os.Stdout),
		openURL:     openBrowser,
		runEditor:   runEditorProcess,
		findEditors: findInstalledEditors,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(app.run(ctx, os.Args[1:]))
}

// errUsage marks a mistake in how the command was called.
var errUsage = errors.New("usage")

func (c *cli) run(ctx context.Context, args []string) int {
	lang = c.language()
	if len(args) == 0 {
		fmt.Fprint(c.stdout, msg("usage"))
		return 0
	}
	command, rest := args[0], args[1:]
	var err error
	switch command {
	case "help", "-h", "--help":
		fmt.Fprint(c.stdout, msg("usage"))
		return 0
	case "version", "--version":
		fmt.Fprintln(c.stdout, "plainmote "+version)
		return 0
	case "login":
		err = c.login(ctx, rest)
	case "logout":
		err = c.logout(ctx, rest)
	case "whoami":
		err = c.whoamiCommand(ctx, rest)
	case "server":
		err = c.serverCommand(rest)
	case "config":
		err = c.configCommand(rest)
	case "ls", "list":
		err = c.ls(ctx, rest)
	case "cat":
		err = c.cat(ctx, rest)
	case "edit":
		err = c.edit(ctx, rest)
	case "push":
		err = c.push(ctx, rest)
	default:
		fmt.Fprintln(c.stderr, msg("error_prefix")+msg("unknown_command", command))
		return 2
	}
	switch {
	case err == nil:
		return 0
	case errors.Is(err, context.Canceled):
		fmt.Fprintln(c.stderr, msg("interrupted"))
		return 130
	case errors.Is(err, errUsage):
		return 2
	case errors.Is(err, errKept):
		return 1
	}
	fmt.Fprintln(c.stderr, msg("error_prefix")+err.Error())
	return 1
}

// flags parses a command's options, which may come before or after its
// arguments, and returns the arguments. Every command takes --server.
func (c *cli) flags(name string, args []string, define func(*flag.FlagSet)) ([]string, string, error) {
	set := flag.NewFlagSet(name, flag.ContinueOnError)
	set.SetOutput(c.stderr)
	server := set.String("server", "", "server address")
	if define != nil {
		define(set)
	}
	var positional []string
	for len(args) > 0 {
		if args[0] == "--" {
			positional = append(positional, args[1:]...)
			break
		}
		if err := set.Parse(args); err != nil {
			return nil, "", errUsage
		}
		args = set.Args()
		if len(args) > 0 && args[0] != "--" {
			positional = append(positional, args[0])
			args = args[1:]
		}
	}
	return positional, *server, nil
}

// language is PLAINMOTE_LANG, then the saved choice, then the locale.
func (c *cli) language() int {
	choice := strings.ToLower(c.getenv("PLAINMOTE_LANG"))
	if choice == "" {
		if path, err := c.credentialsPath(); err == nil {
			if creds, err := loadCredentials(path); err == nil {
				choice = creds.Language
			}
		}
	}
	switch choice {
	case "zh":
		return 1
	case "en":
		return 0
	}
	return detectLanguage(c.getenv)
}

func (c *cli) credentialsPath() (string, error) {
	dir, err := configDir(c.getenv)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "credentials"), nil
}

// session is the server and sign-in a command uses: --server, then
// PLAINMOTE_SERVER, then the saved choice, then the default; the token from
// PLAINMOTE_TOKEN or the saved sign-in for that server.
type session struct {
	server string
	path   string
	creds  credentials
	token  string
}

func (c *cli) session(flagServer string) (session, error) {
	path, err := c.credentialsPath()
	if err != nil {
		return session{}, err
	}
	creds, err := loadCredentials(path)
	if err != nil {
		return session{}, err
	}
	server := flagServer
	if server == "" {
		server = c.getenv("PLAINMOTE_SERVER")
	}
	if server == "" {
		server = creds.Server
	}
	if server == "" {
		server = defaultServer
	}
	server, err = normalizeServer(server)
	if err != nil {
		return session{}, err
	}
	token := c.getenv("PLAINMOTE_TOKEN")
	if token == "" {
		token = creds.Servers[server].Token
	}
	return session{server: server, path: path, creds: creds, token: token}, nil
}

// signedIn is a session that must have a token.
func (c *cli) signedIn(flagServer string) (session, *client, error) {
	s, err := c.session(flagServer)
	if err != nil {
		return s, nil, err
	}
	if s.token == "" {
		return s, nil, fmt.Errorf("%s", msg("not_signed_in", s.server))
	}
	return s, newClient(s.server, s.token), nil
}

func (c *cli) needArgs(args []string, count int, command, what string) error {
	if len(args) != count {
		fmt.Fprintln(c.stderr, msg("error_prefix")+msg("needs_argument", command, what))
		return errUsage
	}
	return nil
}

// ask reads one answer from the person at the terminal.
func (c *cli) ask(prompt string) string {
	fmt.Fprint(c.stderr, prompt)
	line, _ := bufio.NewReader(c.stdin).ReadString('\n')
	return strings.ToLower(strings.TrimSpace(line))
}

func isTerminal(file *os.File) bool {
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

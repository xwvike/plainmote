package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"

	"plainmote/internal/linediff"
)

// errKept is a failure after which the edit is kept in its temporary file;
// what to do with it has already been said.
var errKept = errors.New("edit kept")

// waitFlags are the editors that return at once unless told to wait for the
// file to be closed, and how to tell them.
var waitFlags = map[string]string{
	"code": "--wait", "code-insiders": "--wait", "codium": "--wait", "cursor": "--wait",
	"windsurf": "--wait", "zed": "--wait", "zeditor": "--wait", "subl": "--wait",
	"sublime_text": "--wait", "mate": "--wait", "atom": "--wait",
}

// editorCommand is the editor chosen without asking anyone: what
// chooseEditor does for a script, given only the environment.
func editorCommand(getenv func(string) string) []string {
	return (&cli{getenv: getenv}).chooseEditor(&session{})
}

func hasWait(args []string) bool {
	for _, arg := range args {
		if arg == "--wait" || arg == "-w" {
			return true
		}
	}
	return false
}

// isWindowed reports an editor that opens a window of its own rather than
// taking over the terminal - the one to tell to close its tab.
func isWindowed(command []string) bool {
	program := strings.TrimSuffix(strings.ToLower(filepath.Base(command[0])), ".exe")
	_, ok := waitFlags[program]
	return ok || program == "notepad"
}

// splitCommand cuts an editor setting into words the way a shell would for
// the simple cases people write: spaces separate, quotes group.
func splitCommand(value string) []string {
	var words []string
	var word strings.Builder
	var quote rune
	inWord := false
	for _, r := range value {
		switch {
		case quote != 0 && r == quote:
			quote = 0
		case quote != 0:
			word.WriteRune(r)
		case r == '"' || r == '\'':
			quote, inWord = r, true
		case r == ' ' || r == '\t':
			if inWord {
				words = append(words, word.String())
				word.Reset()
				inWord = false
			}
		default:
			word.WriteRune(r)
			inWord = true
		}
	}
	if inWord {
		words = append(words, word.String())
	}
	return words
}

// runEditorProcess runs the editor attached to this terminal. An interrupt
// belongs to the editor while it runs - vim reads Ctrl+C itself - so it is
// not allowed to end the command line underneath it.
func runEditorProcess(command []string) error {
	signal.Ignore(os.Interrupt)
	defer signal.Reset(os.Interrupt)
	editor := exec.Command(command[0], command[1:]...)
	editor.Stdin, editor.Stdout, editor.Stderr = os.Stdin, os.Stdout, os.Stderr
	return editor.Run()
}

// openBrowser tries to show an address, where there is a screen to show it
// on. Failing is fine: the address is printed either way.
func openBrowser(address string) bool {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", address)
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", address)
	default:
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return false
		}
		command = exec.Command("xdg-open", address)
	}
	return command.Start() == nil
}

// tempName is the file the editor opens: the resource's own filename, so the
// editor picks the right highlighting, reduced to a plain base name.
func tempName(r resource) string {
	for _, candidate := range []string{r.Filename, r.Name} {
		name := filepath.Base(strings.TrimSpace(candidate))
		if name != "" && name != "." && name != ".." && name != string(filepath.Separator) && !strings.ContainsAny(name, `/\:`) {
			return name
		}
	}
	return "resource.txt"
}

// edit downloads a resource into a private temporary directory, opens it in
// the person's editor, and saves it as a new version once the editor exits -
// against the version it was downloaded at, so a change saved elsewhere in
// the meantime is reported instead of overwritten. Nothing the person typed
// is thrown away: whenever it cannot be saved, the file stays where it is.
func (c *cli) edit(ctx context.Context, args []string) error {
	args, server, err := c.flags("edit", args, nil)
	if err != nil {
		return err
	}
	if err := c.needArgs(args, 1, "edit", "<resource>"); err != nil {
		return err
	}
	s, api, err := c.signedIn(server)
	if err != nil {
		return err
	}
	target, err := api.resolve(ctx, args[0])
	if err != nil {
		return err
	}
	switch {
	case target.Remote:
		return fmt.Errorf("%s", msg("is_reference", target.label()))
	case target.Encrypted:
		return fmt.Errorf("%s", msg("encrypted", target.label()))
	case target.ExpiresAt != nil:
		return fmt.Errorf("%s", msg("quick_share_read_only", target.label()))
	case !target.Editable:
		return fmt.Errorf("%s", msg("not_editable", target.label()))
	}
	current, err := api.read(ctx, target.ID)
	if err != nil {
		return err
	}

	dir, err := os.MkdirTemp("", "plainmote-")
	if err != nil {
		return err
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(dir)
		}
	}()
	file := filepath.Join(dir, tempName(target))
	if err := os.WriteFile(file, current.Body, 0o600); err != nil {
		return err
	}
	editor := c.chooseEditor(&s)
	fmt.Fprintln(c.stderr, msg("edit_downloaded", api.host(), tempName(target), current.Version, sizeText(int64(len(current.Body))), filepath.Base(editor[0])))
	if isWindowed(editor) {
		fmt.Fprintln(c.stderr, msg("edit_close_hint"))
	}
	files := []string{file}
	for {
		if err := c.runEditor(append(append([]string{}, editor...), files...)); err != nil {
			keep = true
			fmt.Fprintln(c.stderr, msg("error_prefix")+msg("edit_editor_failed", err))
			c.sayKept(file, target.ID)
			return errKept
		}
		edited, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		if bytes.Equal(edited, current.Body) {
			fmt.Fprintln(c.stdout, msg("edit_unchanged"))
			return nil
		}
		result, err := api.write(ctx, target.ID, edited, current.Version, current.Encoding)
		var refusal *apiError
		if errors.As(err, &refusal) && refusal.Code == "conflict" {
			next, again, err := c.conflict(ctx, api, target, edited, file, refusal.CurrentVersion)
			if err != nil {
				keep = true
				if !errors.Is(err, errKept) {
					fmt.Fprintln(c.stderr, msg("error_prefix")+err.Error())
				}
				c.sayKept(file, target.ID)
				return errKept
			}
			if again {
				current, files = next.content, []string{file, file + ".mine"}
				continue
			}
			result = next.saved
		} else if err != nil {
			keep = true
			fmt.Fprintln(c.stderr, msg("error_prefix")+err.Error())
			c.sayKept(file, target.ID)
			return errKept
		}
		fmt.Fprintln(c.stdout, msg("edit_saved", result.Version, api.host()))
		if result.Trimmed > 0 {
			fmt.Fprintln(c.stderr, msg("edit_trimmed", result.Trimmed))
		}
		return nil
	}
}

func (c *cli) sayKept(file, id string) {
	fmt.Fprintln(c.stderr, msg("edit_kept", file))
	fmt.Fprintln(c.stderr, msg("edit_kept_push", file, id))
}

// conflicted is what a conflict was resolved into: content to edit again, or
// the result of saving over.
type conflicted struct {
	content
	saved saveResult
}

// conflict reports a save refused because the resource moved on, shows how
// the person's edit differs from what is current now, and lets them choose:
// save over it, edit again on top of it, or keep the file. Without someone
// at the terminal to ask, the file is kept.
func (c *cli) conflict(ctx context.Context, api *client, target resource, edited []byte, file string, currentVersion int) (conflicted, bool, error) {
	fmt.Fprintln(c.stderr, msg("conflict", currentVersion))
	latest, err := api.read(ctx, target.ID)
	if err != nil {
		return conflicted{}, false, err
	}
	fmt.Fprintln(c.stderr)
	fmt.Fprintln(c.stderr, msg("conflict_diff", latest.Version))
	c.printDiff(string(latest.Body), string(edited))
	fmt.Fprintln(c.stderr)
	if !c.interactive {
		return conflicted{}, false, errKept
	}
	for {
		switch c.ask(msg("conflict_prompt", latest.Version, latest.Version)) {
		case "o":
			saved, err := api.write(ctx, target.ID, edited, latest.Version, latest.Encoding)
			return conflicted{saved: saved}, false, err
		case "r":
			mine := file + ".mine"
			if err := os.WriteFile(mine, edited, 0o600); err != nil {
				return conflicted{}, false, err
			}
			if err := os.WriteFile(file, latest.Body, 0o600); err != nil {
				return conflicted{}, false, err
			}
			fmt.Fprintln(c.stderr, msg("conflict_reedit", latest.Version, filepath.Base(mine)))
			return conflicted{content: latest}, true, nil
		case "k", "":
			return conflicted{}, false, errKept
		}
	}
}

// printDiff shows the change as a unified diff, briefly: a conflict report,
// not a review.
func (c *cli) printDiff(before, after string) {
	const shown = 40
	diff := linediff.Compare(before, after, false)
	if diff.TooLarge {
		fmt.Fprintln(c.stderr, msg("conflict_too_big"))
		return
	}
	for i, line := range diff.Lines {
		if i == shown {
			fmt.Fprintln(c.stderr, msg("conflict_more", len(diff.Lines)-shown))
			return
		}
		switch line.Kind {
		case "-":
			fmt.Fprintln(c.stderr, "- "+clean(line.Text))
		case "+":
			fmt.Fprintln(c.stderr, "+ "+clean(line.Text))
		case "=":
			fmt.Fprintln(c.stderr, "  "+clean(line.Text))
		case "fold":
			fmt.Fprintf(c.stderr, "  … (%d)\n", line.Skipped)
		}
	}
}

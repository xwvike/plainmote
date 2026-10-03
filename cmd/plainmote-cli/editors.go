package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// editorChoice is an editor found on this machine: what to call it, and the
// command that opens a file in it and waits for it to be closed.
type editorChoice struct {
	Name    string
	Command string
}

// editorCandidates are the editors looked for, in the order they are offered.
var editorCandidates = []struct{ name, program, wait string }{
	{"VS Code", "code", " --wait"},
	{"Cursor", "cursor", " --wait"},
	{"Windsurf", "windsurf", " --wait"},
	{"Zed", "zed", " --wait"},
	{"Zed", "zeditor", " --wait"},
	{"Sublime Text", "subl", " --wait"},
	{"Neovim", "nvim", ""},
	{"Vim", "vim", ""},
	{"Helix", "hx", ""},
	{"Micro", "micro", ""},
	{"Nano", "nano", ""},
	{"Emacs", "emacs", ""},
}

// appBundles are where macOS keeps the command line tools of editors whose
// own installers do not put them on the PATH.
var appBundles = []struct{ name, path, wait string }{
	{"VS Code", "Visual Studio Code.app/Contents/Resources/app/bin/code", " --wait"},
	{"Cursor", "Cursor.app/Contents/Resources/app/bin/cursor", " --wait"},
	{"Windsurf", "Windsurf.app/Contents/Resources/app/bin/windsurf", " --wait"},
	{"Zed", "Zed.app/Contents/MacOS/cli", " --wait"},
	{"Sublime Text", "Sublime Text.app/Contents/SharedSupport/bin/subl", " --wait"},
}

// findInstalledEditors lists the editors this machine has, each once.
func findInstalledEditors() []editorChoice {
	var found []editorChoice
	seen := map[string]bool{}
	add := func(name, command string) {
		if !seen[name] {
			seen[name] = true
			found = append(found, editorChoice{Name: name, Command: command})
		}
	}
	for _, candidate := range editorCandidates {
		if _, err := exec.LookPath(candidate.program); err == nil {
			add(candidate.name, candidate.program+candidate.wait)
		}
	}
	if runtime.GOOS == "darwin" {
		home, _ := os.UserHomeDir()
		for _, bundle := range appBundles {
			for _, root := range []string{"/Applications", filepath.Join(home, "Applications")} {
				path := filepath.Join(root, bundle.path)
				if info, err := os.Stat(path); err == nil && !info.IsDir() {
					add(bundle.name, strconv.Quote(path)+bundle.wait)
				}
			}
		}
	}
	if runtime.GOOS == "windows" {
		add("Notepad", "notepad")
	}
	return found
}

// chooseEditor is the editor edit opens: PLAINMOTE_EDITOR, then the saved
// choice, then $VISUAL and $EDITOR. With none of them set, a person at the
// terminal picks one of the editors found here, and the choice is kept; a
// script gets vi, or Notepad on Windows.
func (c *cli) chooseEditor(s *session) []string {
	for _, setting := range []string{c.getenv("PLAINMOTE_EDITOR"), s.creds.Editor, c.getenv("VISUAL"), c.getenv("EDITOR")} {
		if command := splitCommand(setting); len(command) > 0 {
			return withWait(command)
		}
	}
	if c.interactive && c.findEditors != nil {
		if choices := c.findEditors(); len(choices) > 0 {
			choice := c.pickEditor(choices)
			s.creds.Editor = choice.Command
			if err := s.creds.save(s.path); err == nil {
				fmt.Fprintln(c.stderr, msg("editor_saved", choice.Name))
			}
			return withWait(splitCommand(choice.Command))
		}
	}
	if runtime.GOOS == "windows" {
		return []string{"notepad"}
	}
	return []string{"vi"}
}

func (c *cli) pickEditor(choices []editorChoice) editorChoice {
	fmt.Fprintln(c.stderr, msg("editor_pick"))
	for i, choice := range choices {
		fmt.Fprintf(c.stderr, "  %d) %s\n", i+1, choice.Name)
	}
	for attempt := 0; attempt < 3; attempt++ {
		answer := c.ask(msg("editor_number", len(choices)))
		if answer == "" {
			return choices[0]
		}
		if n, err := strconv.Atoi(answer); err == nil && n >= 1 && n <= len(choices) {
			return choices[n-1]
		}
	}
	return choices[0]
}

// withWait adds the wait flag for editors that return at once without it.
func withWait(command []string) []string {
	program := strings.TrimSuffix(strings.ToLower(filepath.Base(command[0])), ".exe")
	if flag, ok := waitFlags[program]; ok && !hasWait(command[1:]) {
		command = append(command, flag)
	}
	return command
}

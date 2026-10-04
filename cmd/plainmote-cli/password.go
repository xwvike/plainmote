package main

import (
	"fmt"
	"os"
	"runtime"

	"golang.org/x/term"
)

// readTerminalPassword asks for a password on the terminal itself, without
// echo - not on standard input, which may be the file being pushed.
func readTerminalPassword(prompt string) (string, error) {
	name := "/dev/tty"
	if runtime.GOOS == "windows" {
		name = "CONIN$"
	}
	tty, err := os.OpenFile(name, os.O_RDWR, 0)
	if err != nil {
		return "", fmt.Errorf("%s", msg("needs_master_password"))
	}
	defer tty.Close()
	fmt.Fprint(os.Stderr, prompt)
	password, err := term.ReadPassword(int(tty.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("%s", msg("needs_master_password"))
	}
	return string(password), nil
}

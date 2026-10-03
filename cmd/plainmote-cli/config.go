package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// defaultServer is used until a server is set, by the installer or by
// `plainmote server`.
const defaultServer = "https://plainmote.link"

// credentials is the one file the command line keeps: which server to talk
// to, and the sign-in for each server it has signed in to. It holds tokens,
// so it is written readable by its owner only.
type credentials struct {
	Server  string             `json:"server,omitempty"`
	Servers map[string]account `json:"servers,omitempty"`
}

type account struct {
	Token     string    `json:"token"`
	Login     string    `json:"login"`
	Scope     string    `json:"scope"`
	ExpiresAt time.Time `json:"expires_at"`
}

// configDir follows each system's convention: $XDG_CONFIG_HOME or
// ~/.config on Linux and macOS - where command lines usually keep theirs -
// and %APPDATA% on Windows.
func configDir(getenv func(string) string) (string, error) {
	if dir := getenv("PLAINMOTE_CONFIG_DIR"); dir != "" {
		return dir, nil
	}
	if runtime.GOOS == "windows" {
		if dir := getenv("APPDATA"); dir != "" {
			return filepath.Join(dir, "plainmote"), nil
		}
	}
	if dir := getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "plainmote"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "plainmote"), nil
}

func loadCredentials(path string) (credentials, error) {
	var creds credentials
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return creds, nil
	}
	if err != nil {
		return creds, err
	}
	if err := json.Unmarshal(data, &creds); err != nil {
		return creds, fmt.Errorf("%s: %w", path, err)
	}
	// Tighten a file someone loosened: it holds tokens.
	if info, err := os.Stat(path); err == nil && runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		_ = os.Chmod(path, 0o600)
	}
	return creds, nil
}

// save writes the file whole and then moves it into place, so an interrupted
// write never leaves half a file - and never a moment where it is readable by
// anyone else.
func (c credentials) save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".credentials-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if err := temp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		temp.Close()
		return err
	}
	if _, err := temp.Write(append(data, '\n')); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), path)
}

// normalizeServer accepts a server address in the forms people type it and
// returns scheme://host[:port]. Plain http is refused except for this
// machine: a token would otherwise cross the network readable.
func normalizeServer(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("%s", msg("bad_server", value))
	}
	if !strings.Contains(value, "://") {
		value = "https://" + value
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" {
		return "", fmt.Errorf("%s", msg("bad_server", value))
	}
	host := parsed.Hostname()
	local := host == "localhost" || host == "127.0.0.1" || host == "::1"
	switch {
	case parsed.Scheme == "https":
	case parsed.Scheme == "http" && local:
	default:
		return "", fmt.Errorf("%s", msg("bad_server", value))
	}
	return parsed.Scheme + "://" + parsed.Host, nil
}

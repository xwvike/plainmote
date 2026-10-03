package web

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"text/template"
	"time"

	"plainmote/internal/store"
)

const (
	cliPath          = "/cli"
	cliPowerShell    = "/cli.ps1"
	cliChecksums     = "/cli/checksums.txt"
	cliDownloadPrefx = "/cli/download/"
)

// cliTargets are the builds the image carries, in the order the install page
// lists them.
var cliTargets = []struct{ OS, Arch string }{
	{"darwin", "arm64"}, {"darwin", "amd64"},
	{"linux", "amd64"}, {"linux", "arm64"},
	{"windows", "amd64"}, {"windows", "arm64"},
}

type cliBinary struct {
	OS, Arch string
	Path     string
	Size     int64
	SHA256   string
}

// Platform is how the install page names the system.
func (b cliBinary) Platform() string {
	switch b.OS {
	case "darwin":
		return "macOS"
	case "linux":
		return "Linux"
	case "windows":
		return "Windows"
	}
	return b.OS
}

// File is the name a download is saved under.
func (b cliBinary) File() string {
	if b.OS == "windows" {
		return "plainmote.exe"
	}
	return "plainmote"
}

// cliIndex reads the builds once, on first use: their sizes and sums are what
// the page, the checksum list and both install scripts state, and they cannot
// change while the process runs.
type cliIndex struct {
	once     sync.Once
	binaries []cliBinary
}

func (a *App) cliBinaries() []cliBinary {
	a.cli.once.Do(func() {
		if a.cfg.CLIDir == "" {
			return
		}
		for _, target := range cliTargets {
			name := "plainmote-" + target.OS + "-" + target.Arch
			if target.OS == "windows" {
				name += ".exe"
			}
			path := filepath.Join(a.cfg.CLIDir, name)
			sum, size, err := fileSHA256(path)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "read command line build %s: %v\n", name, err)
				continue
			}
			a.cli.binaries = append(a.cli.binaries, cliBinary{OS: target.OS, Arch: target.Arch, Path: path, Size: size, SHA256: sum})
		}
	})
	return a.cli.binaries
}

func fileSHA256(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	digest := sha256.New()
	size, err := io.Copy(digest, file)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(digest.Sum(nil)), size, nil
}

func (a *App) cliBinary(os, arch string) (cliBinary, bool) {
	for _, binary := range a.cliBinaries() {
		if binary.OS == os && binary.Arch == arch {
			return binary, true
		}
	}
	return cliBinary{}, false
}

// handleCLI is the install page for a browser and the install script for
// everything else: the same address works in an address bar and after curl.
func (a *App) handleCLI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	w.Header().Add("Vary", "Accept")
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		w.Header().Add("Vary", "User-Agent")
		w.Header().Add("Vary", "Sec-CH-UA-Platform")
		data := a.basePage(r, User{})
		data.SignedIn = false
		data.Active = "cli"
		if user, _, ok := a.currentUser(r); ok {
			data = a.basePage(r, user)
			data.Active = "cli"
		}
		data.CLIBinaries = a.cliBinaries()
		data.CLIVersion = a.cfg.Version
		data.CLIPlatform = clientPlatform(r)
		data.Indexable = false
		a.renderTemplate(w, r, http.StatusOK, "cli.html", data)
		return
	}
	a.writeScript(w, r, installScript)
}

// clientPlatform tells which install command to put first: "windows" for
// PowerShell, "macos" or "linux" for the shell script, and "" when it cannot
// be told - a phone, say - and both are shown. The processor does not matter
// here: the script works that out on the machine it runs on.
func clientPlatform(r *http.Request) string {
	switch strings.ToLower(strings.Trim(r.Header.Get("Sec-CH-UA-Platform"), `"`)) {
	case "windows":
		return "windows"
	case "macos":
		return "macos"
	case "linux", "chrome os", "chromium os":
		return "linux"
	case "android", "ios":
		return ""
	}
	agent := r.Header.Get("User-Agent")
	switch {
	case strings.Contains(agent, "Windows"):
		return "windows"
	case strings.Contains(agent, "Android"), strings.Contains(agent, "iPhone"), strings.Contains(agent, "iPad"):
		return ""
	case strings.Contains(agent, "Macintosh"), strings.Contains(agent, "Mac OS X"):
		return "macos"
	case strings.Contains(agent, "Linux"), strings.Contains(agent, "X11"), strings.Contains(agent, "CrOS"):
		return "linux"
	}
	return ""
}

func (a *App) handleCLIPowerShell(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	a.writeScript(w, r, installPowerShell)
}

// scriptSafe is what may stand inside the scripts' single quotes: an address
// and a version. The address comes from PLAINMOTE_PUBLIC_URL, or failing that
// from the request's Host, where a quote is allowed; nothing that could end
// the quotes is written into a script someone pipes into a shell.
var scriptSafe = regexp.MustCompile(`^[A-Za-z0-9:/._\[\]-]*$`)

func (a *App) writeScript(w http.ResponseWriter, r *http.Request, script *template.Template) {
	base := a.baseURL(r)
	if !scriptSafe.MatchString(base) || !scriptSafe.MatchString(a.cfg.Version) {
		writePlainError(w, http.StatusBadRequest, "this address cannot be written into an install script; set PLAINMOTE_PUBLIC_URL")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method == http.MethodHead {
		return
	}
	_ = script.Execute(w, map[string]any{
		"Base": base, "Version": a.cfg.Version, "Binaries": a.cliBinaries(),
	})
}

// handleCLIFiles serves the checksum list and the builds, and hands the
// device page its own handler.
func (a *App) handleCLIFiles(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == deviceVerifyPath {
		a.handleDevicePage(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if r.URL.Path == cliChecksums {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		if r.Method == http.MethodHead {
			return
		}
		for _, binary := range a.cliBinaries() {
			fmt.Fprintf(w, "%s  plainmote-%s-%s\n", binary.SHA256, binary.OS, binary.Arch)
		}
		return
	}
	target, ok := strings.CutPrefix(r.URL.Path, cliDownloadPrefx)
	osName, arch, found := strings.Cut(target, "-")
	binary, known := a.cliBinary(osName, arch)
	if !ok || !found || !known {
		writePlainError(w, http.StatusNotFound, "not found")
		return
	}
	file, err := os.Open(binary.Path)
	if err != nil {
		a.serverError(w, "open command line build", err)
		return
	}
	defer file.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+binary.File()+`"`)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, "", time.Time{}, file)
}

// handleDevicePage is where a command line's sign-in is approved: a code is
// typed in, the request it belongs to is shown, and the person allows or
// denies it. It needs a session like any account page, and every step that
// changes something is a CSRF-checked POST.
func (a *App) handleDevicePage(w http.ResponseWriter, r *http.Request) {
	user, sessionID, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	data := a.basePage(r, user)
	data.Active = "cli"
	data.DeviceStep = "code"
	render := func(status int) { a.renderTemplate(w, r, status, "device.html", data) }
	if r.Method == http.MethodGet {
		// A code in the address is ignored on purpose. Typing it is the step
		// that makes sure the person approving is the one who saw it.
		render(http.StatusOK)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !a.checkCSRF(r, sessionID) {
		writePlainError(w, http.StatusForbidden, "invalid csrf token")
		return
	}
	locale := requestLanguage(r).Locale
	now := time.Now().UTC()
	ip := a.clientIP(r)
	refuse := func() {
		a.limits.codeFailures.allow(user.ID, now)
		a.limits.codeAddress.allow(ip, now)
		data.Error = translate(locale, "device_code_invalid")
		render(http.StatusBadRequest)
	}
	if a.limits.codeFailures.blocked(user.ID, now) || a.limits.codeAddress.blocked(ip, now) {
		data.Error = translate(locale, "device_code_locked")
		render(http.StatusTooManyRequests)
		return
	}
	code, valid := store.NormalizeUserCode(r.FormValue("code"))
	if !valid {
		refuse()
		return
	}
	switch r.FormValue("action") {
	case "lookup":
		grant, err := a.db.PendingDeviceGrant(r.Context(), code, now)
		if errors.Is(err, store.ErrNotFound) {
			refuse()
			return
		}
		if err != nil {
			a.serverError(w, "read device grant", err)
			return
		}
		data.DeviceStep, data.DeviceGrant, data.DeviceCode = "confirm", grant, store.FormatUserCode(code)
		render(http.StatusOK)
	case "approve", "deny":
		approve := r.FormValue("action") == "approve"
		grant, err := a.db.PendingDeviceGrant(r.Context(), code, now)
		if err == nil && grant.ID == r.FormValue("grant") {
			err = a.db.DecideDeviceGrant(r.Context(), user.ID, grant.ID, code, approve, now)
		} else if err == nil {
			err = store.ErrNotFound
		}
		if errors.Is(err, store.ErrNotFound) {
			refuse()
			return
		}
		if err != nil {
			a.serverError(w, "decide device grant", err)
			return
		}
		data.DeviceGrant = grant
		data.DeviceStep = "denied"
		if approve {
			data.DeviceStep = "done"
		}
		render(http.StatusOK)
	default:
		writePlainError(w, http.StatusBadRequest, "unknown action")
	}
}

// installScript is what `curl -fsSL <base>/cli | sh` runs. It is meant to be
// read: short, and doing only what its header says.
var installScript = template.Must(template.New("install.sh").Parse(`#!/bin/sh
# Installs the PlainMote command line from {{.Base}}
#
# It finds out which system this is, downloads the matching build, checks it
# against the SHA-256 written below, copies it to $PLAINMOTE_INSTALL_DIR
# (default ~/.local/bin) and points it at this server. Nothing else.
set -eu

base='{{.Base}}'
version='{{.Version}}'

fail() { printf 'plainmote: %s\n' "$1" >&2; exit 1; }
{{if not .Binaries}}
fail "this server does not offer the command line"
{{end}}
case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) fail "unsupported system $(uname -s); see $base/cli" ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) fail "unsupported architecture $(uname -m); see $base/cli" ;;
esac

case "$os-$arch" in
{{- range .Binaries}}{{if ne .OS "windows"}}
  {{.OS}}-{{.Arch}}) sum='{{.SHA256}}' ;;{{end}}{{end}}
  *) fail "no build for $os-$arch on this server" ;;
esac

command -v curl >/dev/null 2>&1 || fail "curl is required"
if command -v sha256sum >/dev/null 2>&1; then
  digest() { sha256sum "$1" | cut -d ' ' -f 1; }
elif command -v shasum >/dev/null 2>&1; then
  digest() { shasum -a 256 "$1" | cut -d ' ' -f 1; }
else
  fail "sha256sum or shasum is required to check the download"
fi

dir="${PLAINMOTE_INSTALL_DIR:-$HOME/.local/bin}"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
trap 'exit 1' INT TERM

printf 'Downloading plainmote %s for %s-%s\n' "$version" "$os" "$arch"
curl -fsSL "$base/cli/download/$os-$arch" -o "$tmp/plainmote"
[ "$(digest "$tmp/plainmote")" = "$sum" ] || fail "checksum mismatch; nothing was installed"
chmod 755 "$tmp/plainmote"
mkdir -p "$dir"
mv "$tmp/plainmote" "$dir/plainmote"
"$dir/plainmote" server "$base" --if-unset >/dev/null

printf 'Installed %s\n' "$dir/plainmote"
case ":$PATH:" in
  *":$dir:"*) ;;
  *) printf '%s is not on your PATH: add it there, or run %s\n' "$dir" "$dir/plainmote" ;;
esac
printf 'Next: plainmote login\n'
`))

var installPowerShell = template.Must(template.New("cli.ps1").Parse(`# Installs the PlainMote command line from {{.Base}}
#
# It downloads the build for this machine, checks it against the SHA-256
# written below, copies it to $env:PLAINMOTE_INSTALL_DIR (default
# %LOCALAPPDATA%\Programs\plainmote), adds that folder to your user PATH and
# points the command line at this server. Nothing else.
$ErrorActionPreference = 'Stop'
$base = '{{.Base}}'
$sums = @{
{{- range .Binaries}}{{if eq .OS "windows"}}
  'windows-{{.Arch}}' = '{{.SHA256}}'{{end}}{{end}}
}
$arch = if ([System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture -eq 'Arm64') { 'arm64' } else { 'amd64' }
$sum = $sums["windows-$arch"]
if (-not $sum) { throw "No build for windows-$arch on this server." }
$dir = if ($env:PLAINMOTE_INSTALL_DIR) { $env:PLAINMOTE_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'Programs\plainmote' }
$target = Join-Path $dir 'plainmote.exe'
$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ('plainmote-' + [guid]::NewGuid() + '.exe')
try {
  Write-Host "Downloading plainmote {{.Version}} for windows-$arch"
  Invoke-WebRequest -UseBasicParsing -Uri "$base/cli/download/windows-$arch" -OutFile $tmp
  if ((Get-FileHash -Algorithm SHA256 $tmp).Hash.ToLower() -ne $sum) { throw 'Checksum mismatch; nothing was installed.' }
  New-Item -ItemType Directory -Force -Path $dir | Out-Null
  Move-Item -Force $tmp $target
} finally {
  if (Test-Path $tmp) { Remove-Item -Force $tmp }
}
& $target server $base --if-unset | Out-Null
$path = [Environment]::GetEnvironmentVariable('Path', 'User')
if (-not (($path -split ';') -contains $dir)) {
  [Environment]::SetEnvironmentVariable('Path', ($path.TrimEnd(';') + ';' + $dir), 'User')
  Write-Host "Added $dir to your PATH; open a new terminal to use it."
}
Write-Host "Installed $target"
Write-Host 'Next: plainmote login'
`))

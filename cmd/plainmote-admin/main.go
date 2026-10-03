// Command plainmote-admin makes the key pair for PlainMote's admin interface
// and signs requests to it, for use without an admin page:
//
//	plainmote-admin keygen
//	plainmote-admin call -key admin.jwk GET https://example.com/_admin/v1/overview
//	plainmote-admin call -key admin.jwk POST https://example.com/_admin/v1/lookup '{"link":"..."}'
//
// The public key goes into PLAINMOTE_ADMIN_KEYS; the private key stays with
// whoever administers the service and never goes onto the server.
package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// jwk is the private key as WebCrypto imports it (RFC 8037).
type jwk struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	D   string `json:"d,omitempty"`
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "plainmote-admin: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: plainmote-admin keygen | call -key FILE METHOD URL [BODY]")
	}
	switch args[0] {
	case "keygen":
		return keygen(out)
	case "call":
		return call(args[1:], out)
	}
	return fmt.Errorf("unknown command %q", args[0])
}

func keygen(out io.Writer) error {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	key, _ := json.Marshal(jwk{Kty: "OKP", Crv: "Ed25519",
		X: base64.RawURLEncoding.EncodeToString(public),
		D: base64.RawURLEncoding.EncodeToString(private.Seed())})
	sum := sha256.Sum256(public)
	_, err = fmt.Fprintf(out, "key id:  %s\npublic:  %s\nprivate: %s\n\n"+
		"Put the public key in PLAINMOTE_ADMIN_KEYS. Keep the private key (a JWK)\n"+
		"with the admin page or in a file for `call`; it never goes on the server.\n",
		hex.EncodeToString(sum[:])[:16], base64.StdEncoding.EncodeToString(public), key)
	return err
}

func call(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("call", flag.ContinueOnError)
	keyFile := flags.String("key", "", "file holding the private key as a JWK")
	if err := flags.Parse(args); err != nil {
		return err
	}
	rest := flags.Args()
	if *keyFile == "" || len(rest) < 2 || len(rest) > 3 {
		return errors.New("usage: plainmote-admin call -key FILE METHOD URL [BODY]")
	}
	raw, err := os.ReadFile(*keyFile)
	if err != nil {
		return err
	}
	var key jwk
	if err := json.Unmarshal(raw, &key); err != nil || key.Kty != "OKP" || key.Crv != "Ed25519" {
		return errors.New("the key file must hold an Ed25519 JWK")
	}
	seed, err := base64.RawURLEncoding.DecodeString(key.D)
	if err != nil || len(seed) != ed25519.SeedSize {
		return errors.New("the key file has no usable private key")
	}
	private := ed25519.NewKeyFromSeed(seed)

	method, target := strings.ToUpper(rest[0]), rest[1]
	var body []byte
	if len(rest) == 3 {
		body = []byte(rest[2])
	}
	parsed, err := url.Parse(target)
	if err != nil || parsed.Host == "" {
		return fmt.Errorf("%q is not an absolute URL", target)
	}
	request, err := http.NewRequest(method, target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if err := Sign(request, private, body, time.Now()); err != nil {
		return err
	}
	if len(body) > 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	fmt.Fprintf(os.Stderr, "%s\n", response.Status)
	_, err = io.Copy(out, response.Body)
	return err
}

// Sign adds the four admin headers to a request, following docs/admin.md.
func Sign(request *http.Request, private ed25519.PrivateKey, body []byte, now time.Time) error {
	public := private.Public().(ed25519.PublicKey)
	sum := sha256.Sum256(public)
	nonce := make([]byte, 18)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	timestamp := strconv.FormatInt(now.Unix(), 10)
	nonceText := base64.RawURLEncoding.EncodeToString(nonce)
	bodySum := sha256.Sum256(body)
	signing := strings.Join([]string{"PLAINMOTE-ADMIN-V1", request.Method, request.URL.RequestURI(), timestamp, nonceText, hex.EncodeToString(bodySum[:])}, "\n")
	request.Header.Set("X-PlainMote-Key", hex.EncodeToString(sum[:])[:16])
	request.Header.Set("X-PlainMote-Timestamp", timestamp)
	request.Header.Set("X-PlainMote-Nonce", nonceText)
	request.Header.Set("X-PlainMote-Signature", base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, []byte(signing))))
	return nil
}

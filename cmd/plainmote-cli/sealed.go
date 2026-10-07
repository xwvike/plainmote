package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// End-to-end encrypted resources, the same formats the browser makes and
// reads (docs/encryption.md, internal/web/static/keys.js and seal.js):
//
//	master password --PBKDF2-SHA-256--> master key, wraps the account key
//	account key wraps each resource's content key
//	content  "PMr1" | IV | AES-256-GCM, additional data "PMr1"+"pm/content/v1:<id>"
//	metadata {"n","f","t"} as JSON, "PMm1" and "pm/meta/v1:<id>"
//
// A wrapped key is IV (12) | the 32 key bytes encrypted | tag (16), with
// additional data naming what it is for.

const (
	ivBytes      = 12
	keyBytes     = 32
	wrappedBytes = ivBytes + keyBytes + 16
)

var (
	contentMagic = []byte("PMr1")
	metaMagic    = []byte("PMm1")
)

var errWrongPassword = errors.New("wrong master password")

func gcmSeal(key, plain, additional []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	iv := make([]byte, ivBytes)
	if _, err := rand.Read(iv); err != nil {
		return nil, err
	}
	return aead.Seal(iv, iv, plain, additional), nil
}

func gcmOpen(key, sealed, additional []byte) ([]byte, error) {
	if len(sealed) < ivBytes+16 {
		return nil, errors.New("truncated ciphertext")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return aead.Open(nil, sealed[:ivBytes], sealed[ivBytes:], additional)
}

func unwrapKey(wrapped, wrapping []byte, purpose string) ([]byte, error) {
	if len(wrapped) != wrappedBytes {
		return nil, errors.New("malformed wrapped key")
	}
	return gcmOpen(wrapping, wrapped, []byte(purpose))
}

func wrapKey(key, wrapping []byte, purpose string) ([]byte, error) {
	return gcmSeal(wrapping, key, []byte(purpose))
}

func decodeB64(value string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(value)
}

// keyring is the account's master password keyring as the service hands it
// over: everything wrapped.
type keyring struct {
	UserID            string `json:"user_id"`
	KDF               string `json:"kdf"`
	Iterations        int    `json:"iterations"`
	Salt              string `json:"salt"`
	WrappedByPassword string `json:"wrapped_by_password"`
}

// open derives the master key from the password and unwraps the account key
// with it. A wrong password fails AES-GCM, as it does in the browser.
func (k keyring) open(password string) ([]byte, error) {
	if k.KDF != "pbkdf2-sha256" || k.Iterations < 600000 {
		return nil, errors.New("unsupported key derivation")
	}
	salt, err := decodeB64(k.Salt)
	if err != nil {
		return nil, err
	}
	wrapped, err := decodeB64(k.WrappedByPassword)
	if err != nil {
		return nil, err
	}
	master, err := pbkdf2.Key(sha256.New, norm.NFC.String(password), salt, k.Iterations, keyBytes)
	if err != nil {
		return nil, err
	}
	account, err := unwrapKey(wrapped, master, "pm/ak/password/v1:"+k.UserID)
	if err != nil {
		return nil, errWrongPassword
	}
	return account, nil
}

type sealedMeta struct {
	Name     string `json:"n"`
	Filename string `json:"f"`
	Type     string `json:"t"`
}

func sealWith(magic []byte, purpose string, key, plain []byte) ([]byte, error) {
	sealed, err := gcmSeal(key, plain, append(append([]byte{}, magic...), purpose...))
	if err != nil {
		return nil, err
	}
	return append(append([]byte{}, magic...), sealed...), nil
}

func openWith(magic []byte, purpose string, key, blob []byte) ([]byte, error) {
	if !bytes.HasPrefix(blob, magic) {
		return nil, errors.New("not encrypted content")
	}
	return gcmOpen(key, blob[len(magic):], append(append([]byte{}, magic...), purpose...))
}

func sealContent(key []byte, id string, plain []byte) ([]byte, error) {
	return sealWith(contentMagic, "pm/content/v1:"+id, key, plain)
}

func openContent(key []byte, id string, blob []byte) ([]byte, error) {
	return openWith(contentMagic, "pm/content/v1:"+id, key, blob)
}

func sealMetadata(key []byte, id string, meta sealedMeta) ([]byte, error) {
	plain, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	return sealWith(metaMagic, "pm/meta/v1:"+id, key, plain)
}

func openMetadata(key []byte, id string, blob []byte) (sealedMeta, error) {
	plain, err := openWith(metaMagic, "pm/meta/v1:"+id, key, blob)
	if err != nil {
		return sealedMeta{}, err
	}
	var meta sealedMeta
	return meta, json.Unmarshal(plain, &meta)
}

// textLike says whether a type is text - shown in a terminal, edited.
func textLike(contentType string) bool {
	mediaType := strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	if strings.HasPrefix(mediaType, "text/") {
		return true
	}
	for _, word := range []string{"json", "xml", "yaml", "toml", "javascript", "x-sh", "sql", "x-ndjson"} {
		if strings.Contains(mediaType, word) {
			return true
		}
	}
	return false
}

var extensionTypes = map[string]string{
	"json": "application/json", "yaml": "text/yaml", "yml": "text/yaml", "toml": "application/toml", "xml": "application/xml",
	"sh": "text/x-sh", "sql": "text/x-sql", "md": "text/markdown", "csv": "text/csv",
}

// guessType is what content made here is, decided before it is encrypted -
// the service cannot look. UTF-8 is text, typed by its filename; images,
// audio and video are what their bytes say; anything else is a download.
func guessType(filename string, body []byte) string {
	if utf8.Valid(body) && !bytes.ContainsRune(body, 0) {
		extension := strings.ToLower(strings.TrimPrefix(filepath.Ext(filename), "."))
		if known, ok := extensionTypes[extension]; ok {
			return known + "; charset=utf-8"
		}
		return "text/plain; charset=utf-8"
	}
	detected := http.DetectContentType(body)
	for _, family := range []string{"image/", "audio/", "video/"} {
		if strings.HasPrefix(detected, family) && !strings.Contains(detected, "svg") {
			return detected
		}
	}
	return "application/octet-stream"
}

// sealed is one encrypted resource opened: its content key and metadata.
type sealed struct {
	id   string
	key  []byte
	meta sealedMeta
}

// masterPassword is PLAINMOTE_MASTER_PASSWORD, or asked for at the terminal
// without echo.
func (c *cli) masterPassword() (string, error) {
	if password := c.getenv("PLAINMOTE_MASTER_PASSWORD"); password != "" {
		return password, nil
	}
	if c.readPassword == nil {
		return "", errors.New(msg("needs_master_password"))
	}
	return c.readPassword(msg("master_password_prompt"))
}

// unlock reads the keyring and opens the account key; once per command.
func (c *cli) unlock(ctx context.Context, api *client) ([]byte, error) {
	if c.accountKey != nil {
		return c.accountKey, nil
	}
	var ring keyring
	if err := api.getJSON(ctx, "/api/v1/keyring", &ring); err != nil {
		var refusal *apiError
		if errors.As(err, &refusal) && refusal.Code == "not_found" {
			return nil, errors.New(msg("no_master_password"))
		}
		return nil, err
	}
	password, err := c.masterPassword()
	if err != nil {
		return nil, err
	}
	key, err := ring.open(password)
	if errors.Is(err, errWrongPassword) {
		return nil, errors.New(msg("wrong_master_password"))
	}
	if err != nil {
		return nil, err
	}
	c.accountKey = key
	return key, nil
}

// openSealed unwraps a resource's content key and decrypts its metadata.
func (c *cli) openSealed(ctx context.Context, api *client, target resource) (sealed, error) {
	account, err := c.unlock(ctx, api)
	if err != nil {
		return sealed{}, err
	}
	wrapped, err := decodeB64(target.SealedKey)
	if err != nil {
		return sealed{}, err
	}
	key, err := unwrapKey(wrapped, account, "pm/ck/resource/v1:"+target.ID)
	if err != nil {
		return sealed{}, errors.New(msg("sealed_unreadable", target.ID))
	}
	metaBlob, err := decodeB64(target.SealedMeta)
	if err != nil {
		return sealed{}, err
	}
	meta, err := openMetadata(key, target.ID, metaBlob)
	if err != nil {
		return sealed{}, errors.New(msg("sealed_unreadable", target.ID))
	}
	return sealed{id: target.ID, key: key, meta: meta}, nil
}

// read gives back an encrypted resource's content decrypted.
func (s sealed) read(ctx context.Context, api *client) (content, error) {
	current, err := api.read(ctx, s.id)
	if err != nil {
		return content{}, err
	}
	plain, err := openContent(s.key, s.id, current.Body)
	if err != nil {
		return content{}, errors.New(msg("sealed_unreadable", s.id))
	}
	return content{Body: plain, Version: current.Version}, nil
}

// write encrypts and saves new content as the next version.
func (s sealed) write(ctx context.Context, api *client, body []byte, base int) (saveResult, error) {
	blob, err := sealContent(s.key, s.id, body)
	if err != nil {
		return saveResult{}, err
	}
	meta, err := sealMetadata(s.key, s.id, s.meta)
	if err != nil {
		return saveResult{}, err
	}
	header := map[string]string{
		"Content-Type": "application/octet-stream", "If-Match": "*",
		"X-PlainMote-Sealed-Meta": base64.RawURLEncoding.EncodeToString(meta),
	}
	if base > 0 {
		header["If-Match"] = `"v` + strconv.Itoa(base) + `"`
	}
	response, err := api.do(ctx, http.MethodPut, "/api/v1/resources/"+url.PathEscape(s.id)+"/content", bytes.NewReader(blob), header)
	if err != nil {
		return saveResult{}, err
	}
	if response.StatusCode != http.StatusOK {
		return saveResult{}, readError(response)
	}
	defer response.Body.Close()
	var result saveResult
	return result, json.NewDecoder(response.Body).Decode(&result)
}

// createSealed makes a new encrypted resource: a new id, a new content key
// wrapped by the account key, and the content and metadata encrypted under
// them.
func (c *cli) createSealed(ctx context.Context, api *client, name, filename string, body []byte) (resource, error) {
	account, err := c.unlock(ctx, api)
	if err != nil {
		return resource{}, err
	}
	id, err := newUUID()
	if err != nil {
		return resource{}, err
	}
	key := make([]byte, keyBytes)
	if _, err := rand.Read(key); err != nil {
		return resource{}, err
	}
	wrapped, err := wrapKey(key, account, "pm/ck/resource/v1:"+id)
	if err != nil {
		return resource{}, err
	}
	blob, err := sealContent(key, id, body)
	if err != nil {
		return resource{}, err
	}
	meta, err := sealMetadata(key, id, sealedMeta{Name: name, Filename: filename, Type: guessType(filename, body)})
	if err != nil {
		return resource{}, err
	}
	return api.createWith(ctx, map[string]string{
		"id": id, "sealed_key": base64.RawURLEncoding.EncodeToString(wrapped), "sealed_meta": base64.RawURLEncoding.EncodeToString(meta),
	}, blob)
}

// shareSealed makes an encrypted quick share as the home page's box does:
// an encrypted resource whose content key is wrapped by the account key and
// by a new link key, that link key wrapped by the account key for its owner
// to show the link again. The link key comes back for the address after #.
func (c *cli) shareSealed(ctx context.Context, api *client, filename, ttl string, body []byte) (quickShare, []byte, error) {
	account, err := c.unlock(ctx, api)
	if err != nil {
		return quickShare{}, nil, err
	}
	id, err := newUUID()
	if err != nil {
		return quickShare{}, nil, err
	}
	linkID, err := newUUID()
	if err != nil {
		return quickShare{}, nil, err
	}
	key, linkKey := make([]byte, keyBytes), make([]byte, keyBytes)
	if _, err := rand.Read(key); err != nil {
		return quickShare{}, nil, err
	}
	if _, err := rand.Read(linkKey); err != nil {
		return quickShare{}, nil, err
	}
	wrapped, err := wrapKey(key, account, "pm/ck/resource/v1:"+id)
	if err != nil {
		return quickShare{}, nil, err
	}
	forLink, err := wrapKey(key, linkKey, "pm/ck/link/v1")
	if err != nil {
		return quickShare{}, nil, err
	}
	forOwner, err := wrapKey(linkKey, account, "pm/lk/v1:"+linkID)
	if err != nil {
		return quickShare{}, nil, err
	}
	blob, err := sealContent(key, id, body)
	if err != nil {
		return quickShare{}, nil, err
	}
	meta, err := sealMetadata(key, id, sealedMeta{Filename: filename, Type: guessType(filename, body)})
	if err != nil {
		return quickShare{}, nil, err
	}
	encode := base64.RawURLEncoding.EncodeToString
	made, err := api.shareWith(ctx, map[string]string{
		"ttl": ttl, "id": id, "link_id": linkID, "sealed_key": encode(wrapped), "sealed_meta": encode(meta),
		"link_key": encode(forLink), "owner_key": encode(forOwner),
	}, blob)
	return made, linkKey, err
}

// newUUID is a random (version 4) UUID, as the browser's randomUUID makes.
func newUUID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

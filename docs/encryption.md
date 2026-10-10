# End-to-end encryption

English | [简体中文](encryption.zh-CN.md)

PlainMote can encrypt content in the browser so that the service stores only ciphertext. Everything is encrypted under a **master password**: a password only the account owner knows. It protects an account key, which in turn protects the keys of encrypted resources and encrypted quick shares. This document describes it.

Quick shares made before quick-share encryption moved under the master password used the `PMe1` format, with the key only in the link or derived from a code. Those still within their term are delivered until they expire; nothing makes them any more.

Encrypted resources can be viewed, edited, shared and exported in the browser. Quick shares made on the home page with its encryption switch on, or with `plainmote share --encrypt`, are encrypted resources too. The command line reads, edits, lists and creates them, asking for the master password each time or reading it from `PLAINMOTE_MASTER_PASSWORD`; it does not keep the account key.

## Goals

- The operator, the database, the object storage and their backups never hold anything that decrypts content without the master password or the recovery key.
- A person who holds one share link can decrypt that resource and nothing else, and loses access when the link is revoked.
- Changing the master password does not require re-encrypting content.

## Keys

```
master password (typed on the device, never sent)
 └─ PBKDF2 ─▶ master key MK (exists only in memory)
               └─ wraps ─▶ account key AK (random; the service keeps it only wrapped)
recovery key RK ─ wraps ─▶ the same AK

AK ─ wraps ─▶ content key CK (one per encrypted resource, random)
               ├─ encrypts ─▶ the content, name, filename and content type
               └─ wrapped by each link's link key LK
link key LK (one per share link, random, after # in the link)
AK ─ wraps ─▶ LK (so the owner can show the full link again)
```

| Key | Made | Kept | Visible to the service |
| --- | --- | --- | --- |
| Master password | Chosen by the owner, at least 8 characters | Nowhere | No |
| Master key MK | PBKDF2 of the master password and a salt | Nowhere; derived when needed | No |
| Account key AK | Random, generated in the browser when the master password is set | Service: wrapped by MK and wrapped by RK. Browser: after unlocking, as a non-extractable key | Only wrapped |
| Recovery key RK | Random, shown once when the master password is set | By the owner | No |
| Content key CK | Random, when a resource is encrypted | Service: wrapped by AK, and by each link's LK | Only wrapped |
| Link key LK | Random, when a share link is made | In the link after `#`; service: wrapped by AK | Only wrapped |

## Algorithms

All cryptography is done by WebCrypto in the browser, with no third-party library. The command line implements the same formats with the Go standard library.

- **Key derivation**: PBKDF2-HMAC-SHA-256, 600,000 iterations, 16-byte random salt. The algorithm name and the iteration count are stored with the keyring, so the count can be raised without breaking existing keyrings. The password is normalised to Unicode NFC before derivation.
- **Encryption and wrapping**: AES-256-GCM with a fresh random 96-bit IV each time. A wrapped key is `IV (12) | ciphertext (32) | tag (16)`, 60 bytes.
- **Additional data** binds each wrapped key to its purpose and owner, so a wrapped key moved to another account or purpose fails to unwrap:

  | Wrapped key | Additional data |
  | --- | --- |
  | AK by MK | `pm/ak/password/v1:<user ID>` |
  | AK by RK | `pm/ak/recovery/v1:<user ID>` |
  | CK by AK | `pm/ck/resource/v1:<resource ID>` |
  | CK by LK | `pm/ck/link/v1` |
  | LK by AK | `pm/lk/v1:<link ID>` |
  | CK by the key a code derives | `pm/ck/code/v1` |
  | The code, by AK | `pm/code/v1:<link ID>` |
  | Content, encrypted with CK | `PMr1` + `pm/content/v1:<resource ID>` |
  | Metadata, encrypted with CK | `PMm1` + `pm/meta/v1:<resource ID>` |

- **Checking the password**: the service does not check the master password. Unwrapping AK is the check: a wrong password fails AES-GCM authentication.
- **Recovery key**: 256 random bits written in Crockford's base32 (no I, L, O or U), 52 characters in 13 groups of four. Input is case-insensitive; `O` is read as `0`, `I` and `L` as `1`.
- **Non-extractable keys**: once unwrapped for use, AK is a non-extractable `CryptoKey` with the usages `wrapKey` and `unwrapKey` only. Rewrapping it (a new password or recovery key) requires unwrapping it again as extractable, which needs the current password or the recovery key.

## Flows

- **Setting the master password**: the browser generates the salt, AK and RK, wraps AK with MK and with RK, and shows RK. Only after the owner confirms that RK has been saved does it send the salt, the parameters and the two wrapped keys. The keyring is created once per account.
- **Unlocking**: the browser downloads the keyring, derives MK, unwraps AK as non-extractable and keeps it in this site's IndexedDB with the time it was last used.
- **Auto-lock**: the key is removed after it has been idle for the chosen time (5 minutes, 15 minutes, 1 hour or 4 hours; 15 minutes by default), when the owner locks it, or when the owner signs out. A page opened while signed out removes any key it finds. When no key remains, the database itself is removed. Unlocking applies to one browser only.
- **Changing the master password**: the owner enters the current and the new password; the browser unwraps AK as extractable, wraps it with a new MK under a new salt and sends it. Content is unaffected.
- **New recovery key**: the owner enters the master password; the browser wraps AK with a new RK, shows it, and sends it once it has been saved. The old recovery key stops working.
- **Forgotten master password**: the owner enters the recovery key and a new password; the browser unwraps AK with RK and wraps it with the new MK. If both the master password and the recovery key are lost, encrypted content cannot be recovered.
- Every change carries the keyring's version number; a change made against an older version is refused, so two devices cannot overwrite each other.

### Encrypted resources

- **Formats**: content is `PMr1 | IV (12) | ciphertext and tag`; the metadata - `{"n": name, "f": filename, "t": type}` as JSON - is the same with `PMm1`. Each version keeps its own encrypted metadata. Encrypted text is UTF-8. A new encrypted resource's ID, and a link's, is generated in the browser, since the additional data is bound to it; the service checks that it is a fresh UUID.
- **Turning encryption on** (unlocked): the browser generates CK, encrypts the current content and every earlier version one by one, with their metadata, and sends them together with CK wrapped by AK. In one transaction the service replaces the plaintext, empties the name and filename, and revokes every existing share link, as their addresses carry no key; the plaintext objects are then deleted. The versions sent must be exactly the versions there are.
- **Turning encryption off**: the browser decrypts the current content and every earlier version and sends them as plaintext, stored and typed as any save would be. Share links stay and deliver plaintext from then on.
- **Editing**: the browser decrypts into the editor and encrypts again on saving, against the version it started from; any save against an older version is a conflict, as encrypting the same text twice never gives the same bytes. Restoring an earlier version copies its ciphertext and metadata. Copying a version into a new resource decrypts it and encrypts it again under the new resource's own CK.
- **Searching**: the service cannot match encrypted names; once unlocked, the browser decrypts them from a list of the account's encrypted resources and matches them locally.
- A new share link gets a random LK; CK wrapped by LK is stored with the link, and LK wrapped by AK lets the owner show the full link again. A recipient's browser uses the LK after `#` to unwrap CK and then decrypt the content. Links serve the current version as before.
- **Links opened by a code**: instead of a key in the address, a link can be opened by a four-character code from 31 letters and digits that cannot be mistaken for one another, read in either case. The browser derives the link's key from the code in upper case with PBKDF2-HMAC-SHA-256, 600,000 iterations and a 16-byte random salt; the link stores the salt followed by CK wrapped by that key - 76 bytes, where CK wrapped by a random LK is 60 - and the code itself wrapped by AK, so the owner can see it again. WebCrypto wraps only keys, so the code is wrapped as raw key material: `IV (12) | the code's bytes encrypted | tag (16)`, 32 bytes. A recipient's page tells the two kinds apart by that length and asks for the code; a wrong code fails as a wrong key does. The owner switches a link between a key and a code, or to a new code, on the link itself: the browser makes the link's keys again and the service replaces them, keeping the address. Whatever opened the link before - the full address or the old code - no longer does.
- Revoking a link deletes it together with its wrapped CK. Anyone who already opened the link may have kept the content.
- **What a link delivers**: `PMs1`, then the resource ID, CK wrapped by the link's LK and the encrypted metadata, each after a two-byte big-endian length, then the encrypted content. A browser opening the link first receives the decryption page, which costs no use; the page fetches this, which counts as one. Programs receive it as it is.
- **Quick shares**: an encrypted quick share is an encrypted resource that ends with its one link. The browser - or the command line - generates the resource's ID, the link's ID, CK and the link's key (or a code), and sends the encrypted content and metadata with CK wrapped by AK and by the link's key, and the link's key or code wrapped by AK, in one request. The service stores them as a quick share: read only until kept, deleted with its link, counting toward the account's storage. Its owner, once unlocked, can open it, show its link or code again, and keep it as an encrypted resource whose link keeps its term. The home page offers the switch only with a master password, and starts it the way the last quick share from it was sent.
- **Export**: the service's archive holds each encrypted content as `content.sealed`, with `sealed.json` listing every encrypted resource's wrapped CK and encrypted metadata and the account's keyring. On the settings' Data page, once unlocked, the browser decrypts that archive and saves it with the plaintext under the names it really has; locked, the archive is saved as it is.

## What the service stores

| Where | What |
| --- | --- |
| `keyrings` (one row per account) | Algorithm and iteration count, salt, AK wrapped by MK, AK wrapped by RK, idle lock time, version, times |
| `resources` | CK wrapped by AK; the encrypted name, filename and type. The name and filename columns stay empty and the content type is `application/vnd.plainmote.sealed` |
| `resource_versions` | Each encrypted version's encrypted metadata |
| `links` | CK wrapped by LK and LK wrapped by AK; or, for a link opened by a code, the salt and CK wrapped by the key derived from the code, and the code wrapped by AK |

The service still sees resource IDs, owners, ciphertext sizes, creation and update times, the number of versions, share links' terms and use counts, and access history (time, IP address, approximate location, browser).

## Threat model

Protected against:

- The operator, or anyone who reads the database, the object storage or a backup: they obtain ciphertext and wrapped keys only.
- A holder of one share link: one resource, until the link is revoked.

Not protected against:

- Trying every code of a link opened by a code, by someone who has the link or the database: there are about 920,000 codes, each try costing 600,000 PBKDF2 iterations. A code only keeps out someone who glimpses the link; sensitive content should be shared with a key in the address.
- Offline guessing of a weak master password by someone who has the database. Each guess costs 600,000 PBKDF2 iterations; a longer password, such as a phrase of several words, is the defence. The interface recommends at least 12 characters and requires 8.
- A tampered server delivering malicious scripts: encryption in a web page depends on the code the server delivers. The source code is public.
- A script injection on this site while a key is unlocked: it could use the key to decrypt, though not read the key itself. The content security policy allows only the site's own scripts.
- A compromised device, or someone using the browser while it is unlocked.
- The service serving an older version's ciphertext as current.

## Limits

- Encrypted resources can be decrypted only in a browser or the PlainMote command line; programs that read a share link directly receive ciphertext. Configuration meant to be read by programs should not be encrypted.
- Remote resources cannot be encrypted.
- Encrypted text is stored as UTF-8.
- The operator cannot see encrypted content; abuse reports can be handled only from metadata or by taking content down.
- Turning encryption on replaces a resource's plaintext, earlier versions included, but plaintext in database and storage backups remains until those backups expire.

# End-to-end encryption

English | [简体中文](encryption.zh-CN.md)

PlainMote can encrypt content in the browser so that the service stores only ciphertext. There are two forms:

- **Encrypted quick shares**: each quick share gets a random key that travels in the link after `#`, or is derived from a four-character code. This is described in the privacy policy and the account page.
- **Master password**: a password only the account owner knows. It protects an account key, which in turn protects the keys of encrypted resources. This document describes it.

The master password and its keyring are available now. Encrypting resources with it is being built; the sections marked *planned* describe the format it will use.

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

AK ─ wraps ─▶ content key CK (one per encrypted resource, random)           planned
               ├─ encrypts ─▶ the content, name, filename and content type
               └─ wrapped by each link's link key LK
link key LK (one per share link, random, after # in the link)               planned
AK ─ wraps ─▶ LK (so the owner can show the full link again)                 planned
```

| Key | Made | Kept | Visible to the service |
| --- | --- | --- | --- |
| Master password | Chosen by the owner, at least 8 characters | Nowhere | No |
| Master key MK | PBKDF2 of the master password and a salt | Nowhere; derived when needed | No |
| Account key AK | Random, generated in the browser when the master password is set | Service: wrapped by MK and wrapped by RK. Browser: after unlocking, as a non-extractable key | Only wrapped |
| Recovery key RK | Random, shown once when the master password is set | By the owner | No |
| Content key CK | Random, when a resource is encrypted (*planned*) | Service: wrapped by AK, and by each link's LK | Only wrapped |
| Link key LK | Random, when a share link is made (*planned*) | In the link after `#`; service: wrapped by AK | Only wrapped |

## Algorithms

All cryptography is done by WebCrypto in the browser, with no third-party library. The command line will implement the same formats with the Go standard library.

- **Key derivation**: PBKDF2-HMAC-SHA-256, 600,000 iterations, 16-byte random salt. The algorithm name and the iteration count are stored with the keyring, so the count can be raised without breaking existing keyrings. The password is normalised to Unicode NFC before derivation.
- **Encryption and wrapping**: AES-256-GCM with a fresh random 96-bit IV each time. A wrapped key is `IV (12) | ciphertext (32) | tag (16)`, 60 bytes.
- **Additional data** binds each wrapped key to its purpose and owner, so a wrapped key moved to another account or purpose fails to unwrap:

  | Wrapped key | Additional data |
  | --- | --- |
  | AK by MK | `pm/ak/password/v1:<user ID>` |
  | AK by RK | `pm/ak/recovery/v1:<user ID>` |
  | CK by AK (*planned*) | `pm/ck/resource/v1:<resource ID>` |
  | CK by LK (*planned*) | `pm/ck/link/v1:<link ID>` |
  | LK by AK (*planned*) | `pm/lk/v1:<link ID>` |

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

### Encrypted resources (*planned*)

- Turning encryption on for a resource generates CK, encrypts the content and its name, filename and type, sends the ciphertext, and deletes the resource's plaintext earlier versions. Edits are decrypted, changed and encrypted again in the browser. Turning it off decrypts in the browser and saves plaintext.
- A new share link gets a random LK; CK wrapped by LK is stored with the link, and LK wrapped by AK lets the owner show the full link again. A recipient's browser uses the LK after `#` to unwrap CK and then decrypt the content. Links serve the current version as before.
- Revoking a link deletes it together with its wrapped CK. Anyone who already opened the link may have kept the content.

## What the service stores

| Where | What |
| --- | --- |
| `keyrings` (one row per account) | Algorithm and iteration count, salt, AK wrapped by MK, AK wrapped by RK, idle lock time, version, times |
| `resources` (*planned*) | CK wrapped by AK; the encrypted name, filename and type |
| `links` (*planned*) | CK wrapped by LK; LK wrapped by AK |

The service still sees resource IDs, owners, ciphertext sizes, creation and update times, the number of versions, share links' terms and use counts, and access history (time, IP address, approximate location, browser).

## Threat model

Protected against:

- The operator, or anyone who reads the database, the object storage or a backup: they obtain ciphertext and wrapped keys only.
- A holder of one share link: one resource, until the link is revoked.

Not protected against:

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
- Turning encryption on deletes a resource's plaintext earlier versions, but plaintext in database backups remains until those backups expire.

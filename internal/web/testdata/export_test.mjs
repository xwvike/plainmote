// Run by TestExportDecryptsInTheBrowser. Given an archive as the service
// builds it (path in ARCHIVE, the account key's keyring in KEYRING with the
// password "correct horse"), writes the decrypted archive to OUT.
import { readFile, writeFile } from "node:fs/promises";
const keys = await import(process.env.KEYS_MODULE);
const { decryptArchive } = await import(process.env.EXPORT_MODULE);
const ring = JSON.parse(process.env.KEYRING);
ring.iterations = Number(ring.iterations);
const key = await keys.openWithPassword(process.env.USER_ID, ring, "correct horse");
const blob = await decryptArchive(key, new Uint8Array(await readFile(process.env.ARCHIVE)));
await writeFile(process.env.OUT, new Uint8Array(await blob.arrayBuffer()));
console.log("ok");

// Run by TestExportDecryptsInTheBrowser: makes a keyring and an encrypted
// resource with two versions the way the browser does, printed as JSON for
// the Go test to store.
const keys = await import(process.env.KEYS_MODULE);
const seal = await import(process.env.SEAL_MODULE);
const b64 = (bytes) => Buffer.from(bytes).toString("base64url");
const user = process.env.USER_ID, id = process.env.RESOURCE_ID;
const made = await keys.createKeyring(user, "correct horse");
const contentKey = await seal.newContentKey();
const enc = new TextEncoder();
const part = async (text, meta) => ({
  content: b64(await seal.encryptContent(contentKey, id, enc.encode(text))),
  meta: b64(await seal.encryptMeta(contentKey, id, meta)),
});
console.log(JSON.stringify({
  keyring: made.record,
  sealed_key: b64(await seal.wrapContentKey(contentKey, made.key, id)),
  v1: await part("SECRET=1\n", { name: "", filename: "old.env", type: "text/plain; charset=utf-8" }),
  v2: await part("SECRET=2\n", { name: "生产", filename: ".env", type: "text/plain; charset=utf-8" }),
}));

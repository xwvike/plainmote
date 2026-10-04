// Opens an encrypted resource with the browser's own code, for tests that
// check what another implementation wrote: KEYRING (JSON), USER_ID,
// RESOURCE_ID, SEALED_KEY, SEALED_META, CONTENT (base64url) in the
// environment; prints {"meta": ..., "text": ...}. The password is
// "correct horse".
const keys = await import(process.env.KEYS_MODULE);
const seal = await import(process.env.SEAL_MODULE);
const from = (s) => new Uint8Array(Buffer.from(s, "base64url"));
const ring = JSON.parse(process.env.KEYRING);
ring.iterations = Number(ring.iterations);
const account = await keys.openWithPassword(process.env.USER_ID, ring, "correct horse");
const id = process.env.RESOURCE_ID;
const contentKey = await seal.openContentKey(from(process.env.SEALED_KEY), account, id);
const meta = await seal.decryptMeta(contentKey, id, from(process.env.SEALED_META));
const text = new TextDecoder().decode(await seal.decryptContent(contentKey, id, from(process.env.CONTENT)));
console.log(JSON.stringify({ meta, text }));

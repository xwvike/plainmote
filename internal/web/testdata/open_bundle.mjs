// Opens what a link to an encrypted resource delivered with the browser's
// own code, as a recipient's page does: BUNDLE and KEY (base64url) in the
// environment. With KEYRING (JSON), USER_ID, LINK_ID and OWNER_KEY as well,
// it also checks that the link's owner gets the same key back with the
// master password, "correct horse". Prints {"meta": ..., "text": ...,
// "owner": true|false}.
const keys = await import(process.env.KEYS_MODULE);
const seal = await import(process.env.SEAL_MODULE);
const from = (s) => new Uint8Array(Buffer.from(s, "base64url"));
const key = from(process.env.KEY);
const opened = await seal.openBundle(from(process.env.BUNDLE), { key });
let owner = false;
if (process.env.KEYRING) {
  const ring = JSON.parse(process.env.KEYRING);
  ring.iterations = Number(ring.iterations);
  const account = await keys.openWithPassword(process.env.USER_ID, ring, "correct horse");
  const back = await seal.openLinkKey(from(process.env.OWNER_KEY), account, process.env.LINK_ID);
  owner = back.length === key.length && back.every((byte, i) => byte === key[i]);
}
const { content, ...meta } = opened;
console.log(JSON.stringify({ meta, text: new TextDecoder().decode(content), owner }));

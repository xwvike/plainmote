// Run by TestSealModule: an encrypted resource made the way sealed.js makes
// one opens with the account key, and its link's bundle with the link key
// alone.
const keys = await import(process.env.KEYS_MODULE);
const seal = await import(process.env.SEAL_MODULE);
const assert = (ok, what) => { if (!ok) { console.error("FAIL: " + what); process.exit(1); } };
const refuses = async (promise, what) => { try { await promise; } catch { return; } assert(false, what); };
const enc = new TextEncoder(), dec = new TextDecoder();

const user = "11111111-2222-3333-4444-555555555555";
const made = await keys.createKeyring(user, "correct horse");
const account = made.key;
const id = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee";

const contentKey = await seal.newContentKey();
const wrapped = await seal.wrapContentKey(contentKey, account, id);
const meta = await seal.encryptMeta(contentKey, id, { name: "生产 .env", filename: ".env", type: "text/plain; charset=utf-8" });
const blob = await seal.encryptContent(contentKey, id, enc.encode("SECRET=1\n"));
assert(dec.decode(blob.slice(0, 4)) === "PMr1" && dec.decode(meta.slice(0, 4)) === "PMm1", "magic");

const opened = await seal.openContentKey(wrapped, account, id);
assert(dec.decode(await seal.decryptContent(opened, id, blob)) === "SECRET=1\n", "content round trip");
assert((await seal.decryptMeta(opened, id, meta)).name === "生产 .env", "meta round trip");
await refuses(seal.decryptContent(opened, "ffffffff-bbbb-cccc-dddd-eeeeeeeeeeee", blob), "content under another resource id");
await refuses(seal.openContentKey(wrapped, account, "ffffffff-bbbb-cccc-dddd-eeeeeeeeeeee"), "key under another resource id");
await refuses(crypto.subtle.exportKey("raw", opened), "the opened content key must not be exportable");

// A link: its key opens the bundle the service delivers, and the owner
// gets the same key back.
const linkID = "12345678-bbbb-cccc-dddd-eeeeeeeeeeee";
const extractable = await seal.openContentKey(wrapped, account, id, true);
const link = await seal.newLinkKeys(extractable, account, linkID);
const back = await seal.openLinkKey(link.ownerKey, account, linkID);
assert(back.every((byte, i) => byte === link.bytes[i]), "the owner's copy of the link key");
await refuses(seal.openLinkKey(link.ownerKey, account, "00000000-bbbb-cccc-dddd-eeeeeeeeeeee"), "the link key under another link id");

const part = (bytes) => [bytes.length >> 8, bytes.length & 255, ...bytes];
const bundle = new Uint8Array([...enc.encode("PMs1"), ...part(enc.encode(id)), ...part(link.sealedKey), ...part(meta), ...blob]);
assert(seal.isBundle(bundle), "bundle magic");
const delivered = await seal.openBundle(bundle, { key: link.bytes });
assert(dec.decode(delivered.content) === "SECRET=1\n" && delivered.filename === ".env", "the bundle opens with the link key");
await refuses(seal.openBundle(bundle, { key: crypto.getRandomValues(new Uint8Array(32)) }), "another link key");
const swapped = new Uint8Array([...enc.encode("PMs1"), ...part(enc.encode("ffffffff-bbbb-cccc-dddd-eeeeeeeeeeee")), ...part(link.sealedKey), ...part(meta), ...blob]);
await refuses(seal.openBundle(swapped, { key: link.bytes }), "a bundle naming another resource");
assert(!seal.bundleWantsCode(bundle), "a link with its key in the address wants no code");

// A link opened by a code: the code alone opens its bundle, in either
// case; its owner reads the code back; nothing else works.
const codes = new Set(Array.from({ length: 200 }, () => seal.newCode()));
assert([...codes].every((code) => /^[ABCDEFGHJKMNPQRSTUVWXYZ2-9]{4}$/.test(code)) && codes.size > 150, "codes");
const code = seal.newCode();
const codeLink = await seal.newCodeLinkKeys(extractable, account, linkID, code);
assert(codeLink.sealedKey.length === 76 && codeLink.ownerKey.length === 32, "code link key sizes");
assert(await seal.openLinkCode(codeLink.ownerKey, account, linkID) === code, "the owner's copy of the code");
await refuses(seal.openLinkCode(codeLink.ownerKey, account, "00000000-bbbb-cccc-dddd-eeeeeeeeeeee"), "the code under another link id");
const codeBundle = new Uint8Array([...enc.encode("PMs1"), ...part(enc.encode(id)), ...part(codeLink.sealedKey), ...part(meta), ...blob]);
assert(seal.bundleWantsCode(codeBundle), "a code link wants its code");
const byCode = await seal.openBundle(codeBundle, { code: code.toLowerCase() });
assert(dec.decode(byCode.content) === "SECRET=1\n", "the bundle opens with the code");
const wrong = code === "AAAA" ? "BBBB" : "AAAA";
await refuses(seal.openBundle(codeBundle, { code: wrong }), "another code");
await refuses(seal.openBundle(codeBundle, { key: link.bytes }), "a code link opened by a link key");
await refuses(seal.openBundle(codeBundle, {}), "a code link with nothing");
console.log("ok");

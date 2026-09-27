// The browser-side encryption, checked outside a browser: Node has the same
// WebCrypto. Run with `node tools/e2ee/e2ee_test.mjs`.
import assert from "node:assert/strict";

const { seal, open, inspect, toBase64URL, fromBase64URL } = await import("../../internal/web/static/e2ee.js");

const text = (value) => new TextEncoder().encode(value);

// Byte for byte, including CRLF, a BOM-less GBK run and a NUL.
const content = Uint8Array.from([...text("port: 7890\r\n"), 0xd6, 0xd0, 0xce, 0xc4, 0x00, ...text("末尾")]);

{
  const { envelope, key } = await seal(content, "clash.yaml");
  assert.equal(new TextDecoder().decode(envelope.slice(0, 4)), "PMe1");
  assert.equal(envelope[4], 0, "a link-key envelope sets no flag");
  assert.match(key, /^[A-Za-z0-9_-]{43}$/, "the key is 32 bytes of base64url");
  assert.equal(inspect(envelope).passphrase, false);
  const opened = await open(envelope, { key });
  assert.equal(opened.name, "clash.yaml");
  assert.deepEqual(opened.content, content);

  // A different key, a changed byte anywhere, a changed header: all refused.
  const other = toBase64URL(crypto.getRandomValues(new Uint8Array(32)));
  await assert.rejects(open(envelope, { key: other }));
  for (const at of [5, 20, envelope.length - 1]) {
    const tampered = envelope.slice();
    tampered[at] ^= 1;
    await assert.rejects(open(tampered, { key }), `a change at byte ${at} must be refused`);
  }
  await assert.rejects(open(envelope, { key: "" }));
  await assert.rejects(open(envelope, { key: "not base64!" }));
}

{
  const { envelope, key } = await seal(text("secret"), "", "correct horse battery staple");
  assert.equal(key, "", "a passphrase envelope puts nothing in the link");
  assert.equal(envelope[4], 1);
  assert.equal(inspect(envelope).passphrase, true);
  const opened = await open(envelope, { passphrase: "correct horse battery staple" });
  assert.equal(opened.name, "");
  assert.equal(new TextDecoder().decode(opened.content), "secret");
  await assert.rejects(open(envelope, { passphrase: "wrong" }));
}

{
  // Two seals of the same text share nothing: fresh key and IV each time.
  const a = await seal(text("same"), "n");
  const b = await seal(text("same"), "n");
  assert.notEqual(a.key, b.key);
  assert.notDeepEqual(a.envelope, b.envelope);
}

assert.throws(() => inspect(text("PMe2xxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")));
assert.throws(() => inspect(Uint8Array.from([...text("PMe1"), 2, ...new Uint8Array(40)])), "unknown flags are refused");
assert.deepEqual(fromBase64URL(toBase64URL(Uint8Array.of(0, 255, 62, 63))), Uint8Array.of(0, 255, 62, 63));

console.log("e2ee: ok");

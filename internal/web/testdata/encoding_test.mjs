// Run by TestEncodingDetection: reads samples as JSON on stdin and prints,
// for each, the encoding detected and the text decoded with it.
globalThis.document = { documentElement: { style: {} }, querySelectorAll: () => [] };
const { decodeBytes, detectFileEncoding } = await import(process.env.EDITOR_MODULE);
let input = "";
for await (const chunk of process.stdin) input += chunk;
process.stdout.write(JSON.stringify(JSON.parse(input).map((sample) => {
  const bytes = Uint8Array.from(Buffer.from(sample.bytes, "base64"));
  const encoding = detectFileEncoding(bytes);
  let text = null;
  try { text = encoding ? decodeBytes(bytes, encoding) : null; } catch { /* reported as null */ }
  return { encoding, text };
})));

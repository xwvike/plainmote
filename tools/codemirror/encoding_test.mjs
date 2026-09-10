import assert from "node:assert/strict";

// CodeMirror inspects this feature table while its bundle is imported. The
// encoding functions themselves do not need a DOM, and the empty selector
// prevents editor enhancement from running in this smoke test.
globalThis.document = { documentElement: { style: {} }, querySelectorAll: () => [] };

const { decodeBytes, detectFileEncoding, SUPPORTED_ENCODINGS } = await import("../../internal/web/static/editor.js");

const fromHex = (value) => Uint8Array.from(value.match(/../g).map((pair) => Number.parseInt(pair, 16)));

const cases = [
  ["UTF-8 BOM", "efbbbf68656c6c6f", "utf-8bom", "hello"],
  ["UTF-16 LE BOM", "fffe41002d4e0a00", "utf-16le-bom", "A中\n"],
  ["UTF-32 BE BOM", "0000feff0000004100004e2d0000000a", "utf-32be-bom", "A中\n"],
  ["UTF-16 LE without BOM", "6e0061006d0065003a0020002d4e87650a00", "utf-16le", "name: 中文\n"],
  ["UTF-16 BE without BOM", "006e0061006d0065003a00204e2d6587000a", "utf-16be", "name: 中文\n"],
  ["UTF-32 LE without BOM", "6e000000610000006d000000650000003a000000200000002d4e0000876500000a000000", "utf-32le", "name: 中文\n"],
  ["UTF-32 BE without BOM", "0000006e000000610000006d000000650000003a0000002000004e2d000065870000000a", "utf-32be", "name: 中文\n"],
  ["GBK", "d6d0cec4b2e2cad4c5e4d6c3cec4bcfed6d0cec4b2e2cad4", "gbk", "中文测试配置文件中文测试"],
  ["Big5", "a4a4a4e5a4a4a4e5a4a4a4e5a4a4a4e5", "big5", "中文中文中文中文"],
  [
    "Shift JIS",
    "82b182ea82cd93fa967b8cea82cc95b68e9a8352815b836882f08ea993ae94bb92e882b782e982bd82df82cc90dd92e8837483408343838b82c582b78142938c8b9e836d815b836882f08e67977082b582dc82b78142",
    "shift_jis",
    "これは日本語の文字コードを自動判定するための設定ファイルです。東京ノードを使用します。",
  ],
];

for (const [label, encoded, expectedEncoding, expectedText] of cases) {
  const source = fromHex(encoded);
  const detected = detectFileEncoding(source);
  assert.equal(detected, expectedEncoding, `${label} detection`);
  assert.equal(decodeBytes(source, detected), expectedText, `${label} decode`);
}

assert.equal(decodeBytes(fromHex("636166e9"), "windows-1252"), "café");
assert.equal(decodeBytes(fromHex("c5e4d6c39439fc36"), "gb18030"), "配置😀");
assert.throws(() => decodeBytes(fromHex("8130"), "big5"));

for (const encoding of SUPPORTED_ENCODINGS) {
  assert.doesNotThrow(() => decodeBytes(new Uint8Array(), encoding), `${encoding} browser decoder`);
}

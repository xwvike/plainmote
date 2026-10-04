// Reading and writing ZIP archives in the browser, for an export decrypted
// here. Only what the service's own archives use: stored and deflated
// entries, UTF-8 names, no ZIP64. Compression is the browser's own
// CompressionStream; without it, entries are written stored.

const LOCAL = 0x04034b50;
const CENTRAL = 0x02014b50;
const END = 0x06054b50;

const CRC_TABLE = (() => {
  const table = new Uint32Array(256);
  for (let n = 0; n < 256; n += 1) {
    let c = n;
    for (let k = 0; k < 8; k += 1) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
    table[n] = c >>> 0;
  }
  return table;
})();

export function crc32(bytes) {
  let crc = 0xffffffff;
  for (let i = 0; i < bytes.length; i += 1) crc = CRC_TABLE[(crc ^ bytes[i]) & 0xff] ^ (crc >>> 8);
  return (crc ^ 0xffffffff) >>> 0;
}

async function through(bytes, stream) {
  const piped = new Blob([bytes]).stream().pipeThrough(stream);
  return new Uint8Array(await new Response(piped).arrayBuffer());
}

function dosTime(date) {
  const year = Math.max(date.getFullYear(), 1980);
  return {
    time: (date.getHours() << 11) | (date.getMinutes() << 5) | (date.getSeconds() >> 1),
    date: ((year - 1980) << 9) | ((date.getMonth() + 1) << 5) | date.getDate(),
  };
}

function fromDos(time, date) {
  return new Date(1980 + (date >> 9), ((date >> 5) & 15) - 1, date & 31, time >> 11, (time >> 5) & 63, (time & 31) * 2);
}

// readZip gives back every file in an archive: {name, data, modified}.
export async function readZip(bytes) {
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  let end = -1;
  for (let i = bytes.length - 22; i >= Math.max(0, bytes.length - 22 - 0xffff); i -= 1) {
    if (view.getUint32(i, true) === END) {
      end = i;
      break;
    }
  }
  if (end < 0) throw new Error("not a zip archive");
  const count = view.getUint16(end + 10, true);
  let offset = view.getUint32(end + 16, true);
  const decoder = new TextDecoder();
  const files = [];
  for (let n = 0; n < count; n += 1) {
    if (view.getUint32(offset, true) !== CENTRAL) throw new Error("malformed zip archive");
    const method = view.getUint16(offset + 10, true);
    const time = view.getUint16(offset + 12, true);
    const date = view.getUint16(offset + 14, true);
    const compressed = view.getUint32(offset + 20, true);
    const nameLength = view.getUint16(offset + 28, true);
    const extraLength = view.getUint16(offset + 30, true);
    const commentLength = view.getUint16(offset + 32, true);
    const local = view.getUint32(offset + 42, true);
    const name = decoder.decode(bytes.subarray(offset + 46, offset + 46 + nameLength));
    offset += 46 + nameLength + extraLength + commentLength;
    if (view.getUint32(local, true) !== LOCAL) throw new Error("malformed zip archive");
    const start = local + 30 + view.getUint16(local + 26, true) + view.getUint16(local + 28, true);
    const raw = bytes.subarray(start, start + compressed);
    let data;
    if (method === 0) data = raw.slice();
    else if (method === 8) data = await through(raw, new DecompressionStream("deflate-raw"));
    else throw new Error(`unsupported zip method ${method}`);
    if (!name.endsWith("/")) files.push({ name, data, modified: fromDos(time, date) });
  }
  return files;
}

// writeZip makes an archive of {name, data, modified} entries.
export async function writeZip(entries) {
  const encoder = new TextEncoder();
  const deflate = typeof CompressionStream === "function";
  const parts = [];
  const central = [];
  let offset = 0;
  for (const entry of entries) {
    const name = encoder.encode(entry.name);
    const body = deflate ? await through(entry.data, new CompressionStream("deflate-raw")) : entry.data;
    const crc = crc32(entry.data);
    const { time, date } = dosTime(entry.modified || new Date());
    const header = new Uint8Array(30 + name.length);
    const h = new DataView(header.buffer);
    h.setUint32(0, LOCAL, true);
    h.setUint16(4, 20, true);
    h.setUint16(6, 0x0800, true);
    h.setUint16(8, deflate ? 8 : 0, true);
    h.setUint16(10, time, true);
    h.setUint16(12, date, true);
    h.setUint32(14, crc, true);
    h.setUint32(18, body.length, true);
    h.setUint32(22, entry.data.length, true);
    h.setUint16(26, name.length, true);
    header.set(name, 30);
    const record = new Uint8Array(46 + name.length);
    const c = new DataView(record.buffer);
    c.setUint32(0, CENTRAL, true);
    c.setUint16(4, 20, true);
    c.setUint16(6, 20, true);
    c.setUint16(8, 0x0800, true);
    c.setUint16(10, deflate ? 8 : 0, true);
    c.setUint16(12, time, true);
    c.setUint16(14, date, true);
    c.setUint32(16, crc, true);
    c.setUint32(20, body.length, true);
    c.setUint32(24, entry.data.length, true);
    c.setUint16(28, name.length, true);
    c.setUint32(42, offset, true);
    record.set(name, 46);
    parts.push(header, body);
    central.push(record);
    offset += header.length + body.length;
  }
  const centralSize = central.reduce((total, record) => total + record.length, 0);
  const end = new Uint8Array(22);
  const e = new DataView(end.buffer);
  e.setUint32(0, END, true);
  e.setUint16(8, entries.length, true);
  e.setUint16(10, entries.length, true);
  e.setUint32(12, centralSize, true);
  e.setUint32(16, offset, true);
  return new Blob([...parts, ...central, end], { type: "application/zip" });
}

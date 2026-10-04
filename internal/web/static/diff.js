// A line diff for content only the browser can read - an encrypted
// resource's versions. The same algorithm, bounds and output as
// internal/linediff, so a comparison reads the same either way; the Go tests
// run both on the same inputs.

export const CONTEXT = 3;
export const MAX_LINES = 200000;
export const MAX_EDITS = 2000;

function splitLines(text) {
  if (text === "") return [];
  return text.replace(/\n$/, "").split("\n").map((line) => line.replace(/\r$/, ""));
}

function intern(a, b) {
  const ids = new Map();
  const number = (lines) => lines.map((line) => {
    if (!ids.has(line)) ids.set(line, ids.size);
    return ids.get(line);
  });
  return [number(a), number(b)];
}

function removalsFirst(ops) {
  for (let start = 0; start < ops.length;) {
    if (ops[start] === "=") {
      start += 1;
      continue;
    }
    let end = start;
    let removed = 0;
    while (end < ops.length && ops[end] !== "=") {
      if (ops[end] === "-") removed += 1;
      end += 1;
    }
    for (let i = start; i < end; i += 1) ops[i] = i - start < removed ? "-" : "+";
    start = end;
  }
  return ops;
}

function backtrack(trace, n, m) {
  const ops = [];
  let x = n;
  let y = m;
  for (let d = trace.length - 1; d >= 0; d -= 1) {
    const v = trace[d];
    const at = (k) => v[k + d + 1];
    const k = x - y;
    const prevK = k === -d || (k !== d && at(k - 1) < at(k + 1)) ? k + 1 : k - 1;
    const prevX = at(prevK);
    const prevY = prevX - prevK;
    while (x > prevX && y > prevY) {
      ops.push("=");
      x -= 1;
      y -= 1;
    }
    if (d > 0) ops.push(x === prevX ? "+" : "-");
    x = prevX;
    y = prevY;
  }
  return removalsFirst(ops.reverse());
}

function myers(a, b) {
  const n = a.length;
  const m = b.length;
  if (n === 0 && m === 0) return [];
  const limit = Math.min(n + m, MAX_EDITS);
  const offset = limit + 1;
  const v = new Array(2 * limit + 3).fill(0);
  const trace = [];
  for (let d = 0; d <= limit; d += 1) {
    trace.push(v.slice(offset - d - 1, offset + d + 2));
    for (let k = -d; k <= d; k += 2) {
      let x = k === -d || (k !== d && v[offset + k - 1] < v[offset + k + 1]) ? v[offset + k + 1] : v[offset + k - 1] + 1;
      let y = x - k;
      while (x < n && y < m && a[x] === b[y]) {
        x += 1;
        y += 1;
      }
      v[offset + k] = x;
      if (x >= n && y >= m) return backtrack(trace, n, m);
    }
  }
  return null;
}

function diffOps(a, b) {
  let head = 0;
  while (head < a.length && head < b.length && a[head] === b[head]) head += 1;
  let tail = 0;
  while (tail < a.length - head && tail < b.length - head && a[a.length - 1 - tail] === b[b.length - 1 - tail]) tail += 1;
  const [ia, ib] = intern(a.slice(head, a.length - tail), b.slice(head, b.length - tail));
  const middle = myers(ia, ib);
  if (middle === null) return null;
  return [...new Array(head).fill("="), ...middle, ...new Array(tail).fill("=")];
}

// compare is linediff.Compare: {lines, added, removed} or {tooLarge: true}.
// Each line is {kind, old, new, text} or {kind: "fold", skipped}.
export function compare(before, after, full = false) {
  const a = splitLines(before);
  const b = splitLines(after);
  if (a.length + b.length > MAX_LINES) return { tooLarge: true };
  const ops = diffOps(a, b);
  if (ops === null) return { tooLarge: true };
  const rows = [];
  let added = 0;
  let removed = 0;
  let oldLine = 1;
  let newLine = 1;
  for (const op of ops) {
    if (op === "=") {
      rows.push({ kind: "=", old: oldLine, new: newLine, text: a[oldLine - 1] });
      oldLine += 1;
      newLine += 1;
    } else if (op === "-") {
      rows.push({ kind: "-", old: oldLine, new: 0, text: a[oldLine - 1] });
      oldLine += 1;
      removed += 1;
    } else {
      rows.push({ kind: "+", old: 0, new: newLine, text: b[newLine - 1] });
      newLine += 1;
      added += 1;
    }
  }
  if (full) return { lines: rows, added, removed };
  const keep = new Array(rows.length).fill(false);
  rows.forEach((row, i) => {
    if (row.kind === "=") return;
    for (let j = Math.max(i - CONTEXT, 0); j <= Math.min(i + CONTEXT, rows.length - 1); j += 1) keep[j] = true;
  });
  const lines = [];
  let skipped = 0;
  rows.forEach((row, i) => {
    if (keep[i]) {
      if (skipped > 0) {
        lines.push({ kind: "fold", skipped });
        skipped = 0;
      }
      lines.push(row);
    } else {
      skipped += 1;
    }
  });
  if (skipped > 0) lines.push({ kind: "fold", skipped });
  return { lines, added, removed };
}

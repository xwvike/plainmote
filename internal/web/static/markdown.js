// Pure-DOM GFM Markdown renderer for PlainMote.
// Builds every element with document.createElement / textContent (zero innerHTML),
// making raw HTML injection and XSS impossible by construction while working
// offline for both plaintext and end-to-end encrypted (.md) documents.

const MD_EXTENSIONS = new Set(["md", "markdown", "mdx"]);

export function isMarkdownFile(filename, contentType) {
  const clean = String(filename || "").trim().toLowerCase();
  const dot = clean.lastIndexOf(".");
  if (dot > 0 && dot < clean.length - 1) {
    return MD_EXTENSIONS.has(clean.slice(dot + 1));
  }
  const mime = String(contentType || "").split(";", 1)[0].trim().toLowerCase();
  return mime === "text/markdown" || mime === "text/x-markdown";
}

function safeHref(raw) {
  const url = String(raw || "").trim();
  if (!url) return "";
  if (url.startsWith("#")) return url;
  try {
    const parsed = new URL(url);
    if (parsed.protocol === "https:" || parsed.protocol === "http:" || parsed.protocol === "mailto:") {
      return parsed.href;
    }
  } catch {
    // Reject relative or malformed schemes so javascript:/data: can never pass.
  }
  return "";
}

function safeImageSrc(raw) {
  const url = String(raw || "").trim();
  if (!url) return "";
  try {
    const parsed = new URL(url);
    if (parsed.protocol === "https:" || parsed.protocol === "http:") {
      return parsed.href;
    }
  } catch {
    // Ignore invalid image URLs.
  }
  return "";
}

function slugify(text, used) {
  const base = String(text || "")
    .trim()
    .toLowerCase()
    .replace(/[^\p{L}\p{N}\s_-]+/gu, "")
    .replace(/\s+/g, "-")
    .replace(/^-+|-+$/g, "") || "section";
  const count = used.get(base) || 0;
  used.set(base, count + 1);
  return count === 0 ? base : `${base}-${count + 1}`;
}

function splitTableRow(line) {
  let trimmed = line.trim();
  if (trimmed.startsWith("|")) trimmed = trimmed.slice(1);
  if (trimmed.endsWith("|") && !trimmed.endsWith("\\|")) trimmed = trimmed.slice(0, -1);
  const cells = [];
  let current = "";
  let inCode = false;
  for (let i = 0; i < trimmed.length; i++) {
    const ch = trimmed[i];
    if (ch === "\\" && i + 1 < trimmed.length && trimmed[i + 1] === "|") {
      current += "|";
      i += 1;
      continue;
    }
    if (ch === "`") inCode = !inCode;
    if (ch === "|" && !inCode) {
      cells.push(current.trim());
      current = "";
    } else {
      current += ch;
    }
  }
  cells.push(current.trim());
  return cells;
}

function parseTableAlign(line, colCount) {
  if (!line || !line.includes("-")) return null;
  const cells = splitTableRow(line);
  if (cells.length === 0 || (colCount > 0 && cells.length !== colCount)) return null;
  const aligns = [];
  for (const cell of cells) {
    if (!/^:?-{3,}:?$/.test(cell)) return null;
    const left = cell.startsWith(":");
    const right = cell.endsWith(":");
    aligns.push(left && right ? "center" : right ? "right" : left ? "left" : "");
  }
  return aligns;
}

function isHorizontalRule(line) {
  const t = line.trim();
  return /^(?:(?:\*\s*){3,}|(?:-\s*){3,}|(?:_\s*){3,})$/.test(t);
}

function matchFenceOpen(line) {
  const m = /^ {0,3}(`{3,}|~{3,})([^\n]*)$/.exec(line);
  if (!m) return null;
  const marker = m[1];
  const info = m[2].trim();
  // Backtick info strings cannot contain backticks per CommonMark.
  if (marker[0] === "`" && info.includes("`")) return null;
  return { char: marker[0], len: marker.length, lang: info.split(/\s+/)[0] || "" };
}

function isFenceClose(line, fence) {
  const m = /^ {0,3}(`{3,}|~{3,})\s*$/.exec(line);
  return Boolean(m && m[1][0] === fence.char && m[1].length >= fence.len);
}

function matchListItem(line) {
  const m = /^( *)([-*+]|(\d{1,9})[.)])(?: +(.*)|$)/.exec(line);
  if (!m) return null;
  return {
    indent: m[1].length,
    ordered: Boolean(m[3]),
    start: m[3] ? Number(m[3]) : 1,
    marker: m[3] ? "ol" : "ul",
    text: m[4] || "",
  };
}

function isBlockStart(lines, i) {
  const line = lines[i];
  if (!line.trim()) return true;
  if (/^ {0,3}#{1,6}(?:[ \t]+|$)/.test(line)) return true;
  if (matchFenceOpen(line)) return true;
  if (/^ {0,3}>/.test(line)) return true;
  if (isHorizontalRule(line)) return true;
  if (matchListItem(line)) return true;
  if (i + 1 < lines.length && line.includes("|")) {
    const header = splitTableRow(line);
    if (header.length >= 2 && parseTableAlign(lines[i + 1], header.length)) return true;
  }
  return false;
}

// Finds the matching closing bracket/paren for [label](url), handling nested brackets.
function parseBracketLink(text, start) {
  let depth = 0;
  let i = start;
  for (; i < text.length; i++) {
    if (text[i] === "\\" && i + 1 < text.length) { i += 1; continue; }
    if (text[i] === "[") depth += 1;
    else if (text[i] === "]") {
      depth -= 1;
      if (depth === 0) break;
    }
  }
  if (depth !== 0 || text[i + 1] !== "(") return null;
  const label = text.slice(start + 1, i);
  let j = i + 2;
  let parenDepth = 1;
  while (j < text.length && parenDepth > 0) {
    if (text[j] === "\\" && j + 1 < text.length) { j += 2; continue; }
    if (text[j] === "(") parenDepth += 1;
    else if (text[j] === ")") parenDepth -= 1;
    j += 1;
  }
  if (parenDepth !== 0) return null;
  let dest = text.slice(i + 2, j - 1).trim();
  // Strip optional title after destination ("url \"title\"").
  if (dest.startsWith("<") && dest.includes(">")) {
    dest = dest.slice(1, dest.indexOf(">"));
  } else {
    dest = dest.split(/\s+/, 1)[0] || "";
  }
  return { label, dest, end: j };
}

function appendInline(parent, text, ctx) {
  let i = 0;
  let buf = "";
  const flush = () => {
    if (buf) {
      parent.append(document.createTextNode(buf));
      buf = "";
    }
  };

  while (i < text.length) {
    const ch = text[i];

    // Backslash escape for ASCII punctuation.
    if (ch === "\\" && i + 1 < text.length && /[!"#$%&'()*+,\-./:;<=>?@[\\\]^_`{|}~]/.test(text[i + 1])) {
      buf += text[i + 1];
      i += 2;
      continue;
    }

    // Hard break inside paragraph (marked as \n by paragraph joiner).
    if (ch === "\n") {
      flush();
      parent.append(document.createElement("br"));
      i += 1;
      continue;
    }

    // Inline code span: matching run of backticks.
    if (ch === "`") {
      let ticks = 1;
      while (text[i + ticks] === "`") ticks += 1;
      const needle = "`".repeat(ticks);
      const close = text.indexOf(needle, i + ticks);
      if (close !== -1) {
        flush();
        let inner = text.slice(i + ticks, close);
        if (inner.length >= 2 && inner.startsWith(" ") && inner.endsWith(" ") && inner.trim().length > 0) {
          inner = inner.slice(1, -1);
        }
        const code = document.createElement("code");
        code.textContent = inner;
        parent.append(code);
        i = close + ticks;
        continue;
      }
    }

    // Image ![alt](url)
    if (ch === "!" && text[i + 1] === "[") {
      const parsed = parseBracketLink(text, i + 1);
      if (parsed) {
        flush();
        const src = safeImageSrc(parsed.dest);
        if (src) {
          const img = document.createElement("img");
          img.src = src;
          img.alt = parsed.label;
          img.loading = "lazy";
          img.referrerPolicy = "no-referrer";
          parent.append(img);
        } else {
          parent.append(document.createTextNode(parsed.label || parsed.dest));
        }
        i = parsed.end;
        continue;
      }
    }

    // Link [label](url)
    if (ch === "[") {
      const parsed = parseBracketLink(text, i);
      if (parsed) {
        flush();
        const href = safeHref(parsed.dest);
        if (href) {
          const a = document.createElement("a");
          a.href = href;
          if (!href.startsWith("#")) {
            a.target = "_blank";
            a.rel = "noopener noreferrer";
          } else {
            a.dataset.mdAnchor = href.slice(1);
          }
          appendInline(a, parsed.label, ctx);
          parent.append(a);
        } else {
          appendInline(parent, parsed.label, ctx);
        }
        i = parsed.end;
        continue;
      }
    }

    // Autolink <https://...> or <mailto:...>
    if (ch === "<") {
      const m = /^<((?:https?:\/\/|mailto:)[^\s<>]+)>/.exec(text.slice(i));
      if (m) {
        const href = safeHref(m[1]);
        if (href) {
          flush();
          const a = document.createElement("a");
          a.href = href;
          a.target = "_blank";
          a.rel = "noopener noreferrer";
          a.textContent = m[1].replace(/^mailto:/, "");
          parent.append(a);
          i += m[0].length;
          continue;
        }
      }
    }

    // Bare URL autolink https://... or http://...
    if ((ch === "h") && (i === 0 || /[\s([{"'：，。]/.test(text[i - 1]))) {
      const m = /^https?:\/\/[^\s<>"'`]+/.exec(text.slice(i));
      if (m) {
        let raw = m[0];
        while (/[.,;:!?)]$/.test(raw)) {
          if (raw.endsWith(")") && (raw.match(/\(/g)?.length || 0) >= (raw.match(/\)/g)?.length || 0)) break;
          raw = raw.slice(0, -1);
        }
        const href = safeHref(raw);
        if (href) {
          flush();
          const a = document.createElement("a");
          a.href = href;
          a.target = "_blank";
          a.rel = "noopener noreferrer";
          a.textContent = raw;
          parent.append(a);
          i += raw.length;
          continue;
        }
      }
    }

    // Bold + Italic ***text***
    if (text.startsWith("***", i)) {
      const close = text.indexOf("***", i + 3);
      if (close > i + 3) {
        flush();
        const strong = document.createElement("strong");
        const em = document.createElement("em");
        appendInline(em, text.slice(i + 3, close), ctx);
        strong.append(em);
        parent.append(strong);
        i = close + 3;
        continue;
      }
    }

    // Bold **text** or __text__
    if (text.startsWith("**", i) || (text.startsWith("__", i) && (i === 0 || !/\w/.test(text[i - 1])))) {
      const delim = text.slice(i, i + 2);
      const close = text.indexOf(delim, i + 2);
      if (close > i + 2 && text[i + 2] !== " ") {
        flush();
        const strong = document.createElement("strong");
        appendInline(strong, text.slice(i + 2, close), ctx);
        parent.append(strong);
        i = close + 2;
        continue;
      }
    }

    // Strikethrough ~~text~~
    if (text.startsWith("~~", i)) {
      const close = text.indexOf("~~", i + 2);
      if (close > i + 2 && text[i + 2] !== " ") {
        flush();
        const del = document.createElement("del");
        appendInline(del, text.slice(i + 2, close), ctx);
        parent.append(del);
        i = close + 2;
        continue;
      }
    }

    // Italic *text* or _text_
    if ((ch === "*" || (ch === "_" && (i === 0 || !/\w/.test(text[i - 1])))) && text[i + 1] !== ch && text[i + 1] !== " ") {
      let close = i + 1;
      while (close < text.length) {
        close = text.indexOf(ch, close);
        if (close === -1) break;
        if (text[close - 1] !== "\\" && text[close - 1] !== " " && text[close + 1] !== ch) {
          if (ch === "_" && close + 1 < text.length && /\w/.test(text[close + 1])) {
            close += 1;
            continue;
          }
          break;
        }
        close += 1;
      }
      if (close > i + 1) {
        flush();
        const em = document.createElement("em");
        appendInline(em, text.slice(i + 1, close), ctx);
        parent.append(em);
        i = close + 1;
        continue;
      }
    }

    buf += ch;
    i += 1;
  }

  flush();
}

function buildCodeBlock(codeText, lang, ctx) {
  const wrap = document.createElement("div");
  wrap.className = "md-code";

  const head = document.createElement("div");
  head.className = "md-code-head";
  const langEl = document.createElement("span");
  langEl.className = "md-code-lang";
  langEl.textContent = lang || "text";
  const copyBtn = document.createElement("button");
  copyBtn.type = "button";
  copyBtn.className = "md-code-copy";
  copyBtn.textContent = ctx.copyLabel;
  copyBtn.addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText(codeText);
      copyBtn.textContent = ctx.copiedLabel;
      copyBtn.classList.add("copied");
      setTimeout(() => {
        copyBtn.textContent = ctx.copyLabel;
        copyBtn.classList.remove("copied");
      }, 1500);
    } catch {
      // Ignore clipboard failures in restricted contexts.
    }
  });
  head.append(langEl, copyBtn);

  const pre = document.createElement("pre");
  const code = document.createElement("code");
  code.textContent = codeText;
  pre.append(code);

  wrap.append(head, pre);
  return wrap;
}

function parseList(lines, startIndex, baseIndent, ctx) {
  const first = matchListItem(lines[startIndex]);
  const list = document.createElement(first.marker);
  if (first.ordered && first.start !== 1) list.start = first.start;

  let i = startIndex;
  while (i < lines.length) {
    const item = matchListItem(lines[i]);
    if (!item || item.indent < baseIndent) break;
    if (item.indent > baseIndent) {
      // Nested sublist attaches to the previous <li>.
      const prevLi = list.lastElementChild;
      if (!prevLi) break;
      const nested = parseList(lines, i, item.indent, ctx);
      prevLi.append(nested.node);
      i = nested.next;
      continue;
    }
    if (item.marker !== first.marker) break;

    const li = document.createElement("li");
    let text = item.text;
    const task = /^\[([ xX])\](?:\s+(.*)|$)/.exec(text);
    if (task) {
      li.className = "md-task";
      const box = document.createElement("span");
      box.className = "md-check";
      box.setAttribute("aria-hidden", "true");
      if (task[1].toLowerCase() === "x") {
        box.dataset.checked = "1";
        box.textContent = "✓";
      }
      li.append(box);
      text = task[2] || "";
    }

    // Collect continuation lines indented under this item.
    const textSpan = document.createElement("span");
    textSpan.className = "md-li-text";
    const paraLines = [text];
    i += 1;
    while (i < lines.length) {
      const next = lines[i];
      if (!next.trim()) {
        // Peek ahead: if next non-empty line is a sibling or nested list item, continue list.
        let k = i + 1;
        while (k < lines.length && !lines[k].trim()) k += 1;
        const ahead = k < lines.length ? matchListItem(lines[k]) : null;
        if (ahead && ahead.indent >= baseIndent) {
          i = k;
        }
        break;
      }
      const nextItem = matchListItem(next);
      if (nextItem) {
        if (nextItem.indent > baseIndent) {
          break;
        }
        break;
      }
      const leading = next.match(/^ */)[0].length;
      if (leading >= baseIndent + 2 && !matchFenceOpen(next) && !/^ {0,3}>/.test(next)) {
        paraLines.push(next.trim());
        i += 1;
      } else {
        break;
      }
    }
    appendInline(textSpan, paraLines.join(" "), ctx);
    li.append(textSpan);

    // Attach immediately following nested list if indented deeper.
    if (i < lines.length) {
      const sub = matchListItem(lines[i]);
      if (sub && sub.indent > baseIndent) {
        const nested = parseList(lines, i, sub.indent, ctx);
        li.append(nested.node);
        i = nested.next;
      }
    }

    list.append(li);
  }
  return { node: list, next: i };
}

function renderBlocks(parent, lines, ctx) {
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];
    if (!line.trim()) {
      i += 1;
      continue;
    }

    // 1. ATX Heading
    const heading = /^ {0,3}(#{1,6})(?:[ \t]+(.*?))?(?:[ \t]+#+[ \t]*)?$/.exec(line);
    if (heading) {
      const level = heading[1].length;
      const content = (heading[2] || "").trim();
      const h = document.createElement(`h${level}`);
      h.id = slugify(content, ctx.slugs);
      appendInline(h, content, ctx);
      parent.append(h);
      i += 1;
      continue;
    }

    // 2. Fenced Code Block
    const fence = matchFenceOpen(line);
    if (fence) {
      const body = [];
      i += 1;
      while (i < lines.length && !isFenceClose(lines[i], fence)) {
        body.push(lines[i]);
        i += 1;
      }
      if (i < lines.length) i += 1;
      parent.append(buildCodeBlock(body.join("\n"), fence.lang, ctx));
      continue;
    }

    // 3. Horizontal Rule
    if (isHorizontalRule(line)) {
      parent.append(document.createElement("hr"));
      i += 1;
      continue;
    }

    // 4. Blockquote (supports nested markdown blocks)
    if (/^ {0,3}>/.test(line)) {
      const quoted = [];
      while (i < lines.length && /^ {0,3}>/.test(lines[i])) {
        quoted.push(lines[i].replace(/^ {0,3}> ?/, ""));
        i += 1;
      }
      const bq = document.createElement("blockquote");
      renderBlocks(bq, quoted, ctx);
      parent.append(bq);
      continue;
    }

    // 5. GFM Table
    if (i + 1 < lines.length && line.includes("|")) {
      const headerCells = splitTableRow(line);
      const aligns = headerCells.length >= 2 ? parseTableAlign(lines[i + 1], headerCells.length) : null;
      if (aligns) {
        const wrap = document.createElement("div");
        wrap.className = "md-table-wrap";
        const table = document.createElement("table");
        const thead = document.createElement("thead");
        const hr = document.createElement("tr");
        for (let c = 0; c < headerCells.length; c++) {
          const th = document.createElement("th");
          if (aligns[c]) th.style.textAlign = aligns[c];
          appendInline(th, headerCells[c], ctx);
          hr.append(th);
        }
        thead.append(hr);
        table.append(thead);

        const tbody = document.createElement("tbody");
        i += 2;
        while (i < lines.length && lines[i].trim() && lines[i].includes("|") && !isHorizontalRule(lines[i])) {
          const rowCells = splitTableRow(lines[i]);
          const tr = document.createElement("tr");
          for (let c = 0; c < headerCells.length; c++) {
            const td = document.createElement("td");
            if (aligns[c]) td.style.textAlign = aligns[c];
            appendInline(td, rowCells[c] || "", ctx);
            tr.append(td);
          }
          tbody.append(tr);
          i += 1;
        }
        table.append(tbody);
        wrap.append(table);
        parent.append(wrap);
        continue;
      }
    }

    // 6. Ordered / Unordered / Task List
    const listItem = matchListItem(line);
    if (listItem) {
      const built = parseList(lines, i, listItem.indent, ctx);
      parent.append(built.node);
      i = built.next;
      continue;
    }

    // 7. Paragraph
    const para = [];
    while (i < lines.length) {
      const cur = lines[i];
      if (para.length > 0 && isBlockStart(lines, i)) break;
      para.push(cur);
      i += 1;
    }
    const p = document.createElement("p");
    let joined = "";
    for (let k = 0; k < para.length; k++) {
      const raw = para[k];
      if (k === para.length - 1) {
        joined += raw.trim();
      } else if (/ {2,}$/.test(raw) || /\\$/.test(raw)) {
        joined += raw.replace(/(?: {2,}|\\)$/, "").trimStart() + "\n";
      } else {
        joined += raw.trim() + " ";
      }
    }
    appendInline(p, joined, ctx);
    parent.append(p);
  }
}

export function renderMarkdown(source, container, options = {}) {
  container.replaceChildren();
  const text = String(source || "").replace(/\r\n?/g, "\n");
  if (!text.trim()) {
    const empty = document.createElement("p");
    empty.className = "md-empty";
    empty.textContent = options.emptyText || "Nothing to preview yet.";
    container.append(empty);
    return;
  }

  const prose = document.createElement("article");
  prose.className = "md-prose";
  const ctx = {
    slugs: new Map(),
    copyLabel: options.copyLabel || "Copy",
    copiedLabel: options.copiedLabel || "Copied",
  };
  renderBlocks(prose, text.split("\n"), ctx);

  // Smooth in-pane anchor scrolling for #heading links.
  prose.addEventListener("click", (event) => {
    const anchor = event.target.closest("a[data-md-anchor]");
    if (!anchor) return;
    const id = anchor.dataset.mdAnchor;
    if (!id) return;
    const target = prose.querySelector(`#${CSS.escape(id)}`);
    if (target) {
      event.preventDefault();
      target.scrollIntoView({ behavior: "smooth", block: "start" });
    }
  });

  container.append(prose);
}

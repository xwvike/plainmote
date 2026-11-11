const sidebar = document.querySelector("[data-tag-sidebar]");
const search = sidebar.querySelector("[data-tag-search]");
const empty = sidebar.querySelector("[data-tag-empty]");
const form = document.querySelector("#resource-filters");
const error = document.querySelector("#tag-filter-error");
const options = [...sidebar.querySelectorAll("[data-tag-option]")].map((row) => {
  const input = row.querySelector("input");
  return { row, input, name: input.value.toLowerCase() };
});

function filterTags() {
  const query = search.value.trim().toLowerCase();
  let matches = 0;
  for (const { row, name } of options) {
    row.hidden = !name.includes(query);
    if (!row.hidden) matches++;
  }
  empty.hidden = matches > 0;
}

function selectedTags(root) {
  return new Set([...root.querySelectorAll('input[name="tag"]:checked')].map((input) => input.value));
}

function showSelection(tags) {
  for (const { input } of options) input.checked = tags.has(input.value);
}

let applied = selectedTags(sidebar);
let pending;
async function refreshResults(url, fromHistory = false) {
  pending?.abort();
  const request = new AbortController();
  pending = request;
  const results = document.querySelector("#resource-results");
  results.setAttribute("aria-busy", "true");
  error.hidden = true;
  try {
    const response = await fetch(url, {
      headers: { "X-PlainMote-Fragment": "1" },
      signal: request.signal,
    });
    if (response.redirected && new URL(response.url).pathname !== url.pathname) {
      location.assign(response.url);
      return;
    }
    if (!response.ok) throw new Error(`HTTP ${response.status}`);
    const page = new DOMParser().parseFromString(await response.text(), "text/html");
    if (request.signal.aborted) return;
    const ids = ["resource-results", "resource-count", "resource-clear", "tag-clear"];
    const updates = ids.map((id) => [document.getElementById(id), page.getElementById(id)]);
    const nextForm = page.querySelector("#resource-filters");
    const nextTags = page.querySelector("#resource-selected-tags")?.content;
    if (!nextForm || !nextTags || updates.some(([current, next]) => !current || !next)) {
      location.assign(response.url);
      return;
    }
    const scrollLeft = results.querySelector(".table-scroll")?.scrollLeft || 0;
    const [[, nextResults], [count, nextCount], ...links] = updates;
    results.replaceWith(nextResults);
    count.textContent = nextCount.textContent;
    for (const [link, next] of links) {
      if (next.hidden && document.activeElement === link) search.focus({ preventScroll: true });
      link.hidden = next.hidden;
      link.setAttribute("href", next.getAttribute("href"));
    }
    const table = document.querySelector("#resource-results .table-scroll");
    if (table) table.scrollLeft = scrollLeft;
    applied = selectedTags(nextTags);
    showSelection(applied);
    if (fromHistory) {
      for (const name of ["q", "kind", "size"]) form.elements[name].value = nextForm.elements[name].value;
      if (response.url !== location.href) history.replaceState(history.state, "", response.url);
    } else if (response.url !== location.href) {
      history.pushState(null, "", response.url);
    }
    document.dispatchEvent(new Event("plainmote:resource-results"));
  } catch (failure) {
    if (request.signal.aborted) return;
    if (fromHistory) {
      location.reload();
    } else {
      showSelection(applied);
      error.hidden = false;
      console.error("Could not update resource filters", failure);
    }
  } finally {
    if (pending === request) {
      pending = undefined;
      results.removeAttribute("aria-busy");
    }
  }
}

search.hidden = false;
search.addEventListener("input", filterTags);
window.addEventListener("pageshow", filterTags);

sidebar.addEventListener("change", (event) => {
  if (!event.target.matches('input[name="tag"]')) return;
  const url = new URL(form.action);
  url.search = new URLSearchParams(new FormData(form));
  refreshResults(url);
});

sidebar.addEventListener("click", (event) => {
  const link = event.target.closest("#tag-clear");
  if (!link || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
  event.preventDefault();
  const url = new URL(link.href);
  showSelection(new Set());
  refreshResults(url);
});

window.addEventListener("popstate", () => {
  refreshResults(new URL(location.href), true);
});

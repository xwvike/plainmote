const sidebar = document.querySelector("[data-tag-sidebar]");
const search = sidebar.querySelector("[data-tag-search]");
const empty = sidebar.querySelector("[data-tag-empty]");
const list = sidebar.querySelector(".tag-options");
const pendingKey = "plainmote:tag-filter";
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

search.hidden = false;
search.addEventListener("input", filterTags);
window.addEventListener("pageshow", filterTags);

sidebar.addEventListener("change", (event) => {
  const input = event.target;
  if (!input.matches('input[name="tag"]')) return;
  try {
    sessionStorage.setItem(pendingKey, JSON.stringify({
      query: search.value,
      scroll: list.scrollTop,
      tag: input.value,
    }));
  } catch {
    // The form still works when browser storage is unavailable.
  }
  input.form.requestSubmit();
});

// Resume where the user was choosing tags after the results reload.
try {
  const pending = JSON.parse(sessionStorage.getItem(pendingKey));
  sessionStorage.removeItem(pendingKey);
  if (pending) {
    search.value = pending.query;
    filterTags();
    list.scrollTop = pending.scroll;
    options.find(({ input }) => input.value === pending.tag)?.input.focus({ preventScroll: true });
  }
} catch {
  // The page works without a saved position.
}

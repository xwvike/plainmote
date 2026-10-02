// A button with data-copy puts its value on the clipboard, ticks for a moment,
// and has the page's copy-status region say so to a screen reader. Delegated
// from the document, so buttons that arrive later - a refreshed share list,
// an opened dialog - work the same.
document.addEventListener("click", (event) => {
  const button = event.target.closest("[data-copy]");
  if (!button) return;
  const value = button.getAttribute("data-copy");
  const status = document.querySelector("[data-copy-status]");
  if (!navigator.clipboard) {
    window.prompt(status ? status.getAttribute("data-prompt") : "", value);
    return;
  }
  navigator.clipboard.writeText(value).then(() => {
    clearTimeout(button.copiedTimer);
    button.classList.add("copied");
    if (status) {
      status.textContent = "";
      status.textContent = status.getAttribute("data-message");
    }
    button.copiedTimer = setTimeout(() => {
      button.classList.remove("copied");
      if (status) status.textContent = "";
    }, 1400);
  });
});

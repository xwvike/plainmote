const locale = document.documentElement.lang || undefined;

const formatters = {
  minute: new Intl.DateTimeFormat(locale, {
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    hourCycle: "h23",
  }),
  second: new Intl.DateTimeFormat(locale, {
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    hourCycle: "h23",
  }),
  day: new Intl.DateTimeFormat(locale, {
    month: "2-digit",
    day: "2-digit",
  }),
  zone: new Intl.DateTimeFormat(locale, {
    dateStyle: "medium",
    timeStyle: "long",
  }),
};

function localizeTime(element) {
  if (!(element instanceof HTMLTimeElement) || element.dataset.localized === "true") return;
  const value = new Date(element.dateTime);
  if (Number.isNaN(value.getTime())) return;
  const precision = ["minute", "day"].includes(element.dataset.localTime) ? element.dataset.localTime : "second";
  element.textContent = formatters[precision].format(value);
  element.title = formatters.zone.format(value);
  element.dataset.localized = "true";
}

function localizeTree(root) {
  if (root instanceof HTMLTimeElement) localizeTime(root);
  if (root && root.querySelectorAll) {
    for (const element of root.querySelectorAll("time[data-local-time]")) localizeTime(element);
  }
}

localizeTree(document);
new MutationObserver((records) => {
  for (const record of records) {
    for (const node of record.addedNodes) {
      if (node instanceof Element) localizeTree(node);
    }
  }
}).observe(document.documentElement, { childList: true, subtree: true });

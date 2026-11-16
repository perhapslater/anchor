(() => {
  "use strict";
  const inputs = [...document.querySelectorAll("[data-password-warning]")];
  function refresh() {
    for (const input of inputs) {
      const warning = document.getElementById(input.dataset.passwordWarning);
      const length = Array.from(input.value).length;
      warning.hidden = input.disabled || length === 0 || length >= 12;
    }
  }
  for (const input of inputs) {
    input.addEventListener("input", refresh);
    input.addEventListener("change", refresh);
  }
  for (const form of new Set(inputs.map(input => input.form).filter(Boolean))) {
    form.addEventListener("reset", () => queueMicrotask(refresh));
  }
  window.anchorPasswordWarnings = { refresh };
  refresh();
})();

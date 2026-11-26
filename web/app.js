(() => {
  const form = document.getElementById("agent-key-form");
  const panel = document.getElementById("new-agent-key");
  const token = document.getElementById("new-agent-token");
  const copyButton = document.getElementById("copy-agent-key");
  const copyStatus = document.getElementById("copy-agent-key-status");
  const formStatus = document.getElementById("agent-key-form-status");
  const list = document.getElementById("agent-key-list");
  const rowTemplate = document.getElementById("agent-key-row-template");
  const bootstrapForm = document.getElementById("bootstrap-form");
  const bootstrapResult = document.getElementById("bootstrap-result");
  const bootstrapPrompt = document.getElementById("bootstrap-prompt");
  const copyBootstrapButton = document.getElementById("copy-bootstrap-prompt");
  const copyBootstrapStatus = document.getElementById("copy-bootstrap-prompt-status");
  const bootstrapFormStatus = document.getElementById("bootstrap-form-status");

  async function copyField(field, status, copiedText, fallbackText) {
    if (!field || !field.value) return;
    try {
      if (navigator.clipboard && window.isSecureContext) {
        await navigator.clipboard.writeText(field.value);
      } else {
        field.select();
        document.execCommand("copy");
      }
      if (status) status.textContent = copiedText;
    } catch (_) {
      field.focus();
      field.select();
      if (status) status.textContent = fallbackText;
    }
  }

  function reveal(field, resultPanel, status) {
    resultPanel.hidden = false;
    resultPanel.scrollIntoView({ block: "nearest" });
    field.focus();
    if (status) status.textContent = "";
  }

  if (copyButton && token) {
    copyButton.addEventListener("click", async () => {
      await copyField(token, copyStatus, "Copied to clipboard", "Key selected. Press Ctrl+C or Command+C to copy.");
    });
  }

  if (copyBootstrapButton && bootstrapPrompt) {
    copyBootstrapButton.addEventListener("click", async () => {
      await copyField(bootstrapPrompt, copyBootstrapStatus, "Copied to clipboard", "Prompt selected. Press Ctrl+C or Command+C to copy.");
    });
  }

  if (form && token && panel && list && rowTemplate) {
    form.addEventListener("submit", async (event) => {
      event.preventDefault();
      const submit = form.querySelector('button[type="submit"]');
      submit.disabled = true;
      formStatus.textContent = "";
      copyStatus.textContent = "";
      try {
        const response = await fetch(form.action, {
          method: "POST",
          body: new FormData(form),
          headers: { Accept: "application/json" }
        });
        const contentType = response.headers.get("content-type") || "";
        let payload = null;
        if (contentType.includes("application/json")) {
          payload = await response.json();
        } else if (!response.ok) {
          throw new Error(`Could not create key (server returned ${response.status})`);
        } else {
          throw new Error("Your session may have expired. Reload the page and try again.");
        }
        if (!response.ok) throw new Error(payload.error || "Could not create key");

        token.value = payload.token;
        form.reset();

        const row = rowTemplate.content.firstElementChild.cloneNode(true);
        row.querySelector("[data-key-name]").textContent = payload.key.name;
        row.querySelector("[data-key-prefix]").textContent = `raises_agent_${payload.key.prefix}…`;
        if (payload.key.can_mint) row.querySelector("[data-key-account]").hidden = false;
        row.querySelector("[data-key-created]").textContent = `Created ${payload.key.created_at.slice(0, 10)}`;
        row.querySelector("[data-key-revoke]").action = `/settings/agent-keys/${encodeURIComponent(payload.key.id)}/revoke`;
        document.getElementById("no-agent-keys")?.remove();
        list.prepend(row);
        reveal(token, panel, copyStatus);
      } catch (error) {
        formStatus.textContent = error.message || "Could not create key";
      } finally {
        submit.disabled = false;
      }
    });
  }

  if (bootstrapForm && bootstrapPrompt && bootstrapResult) {
    bootstrapForm.addEventListener("submit", async (event) => {
      event.preventDefault();
      const submit = bootstrapForm.querySelector('button[type="submit"]');
      submit.disabled = true;
      bootstrapFormStatus.textContent = "";
      copyBootstrapStatus.textContent = "";
      try {
        const response = await fetch(bootstrapForm.action, {
          method: "POST",
          body: new FormData(bootstrapForm),
          headers: { Accept: "application/json" }
        });
        const bootstrapContentType = response.headers.get("content-type") || "";
        let payload = null;
        if (bootstrapContentType.includes("application/json")) {
          payload = await response.json();
        } else if (!response.ok) {
          throw new Error(`Could not create prompt (server returned ${response.status})`);
        } else {
          throw new Error("Your session may have expired. Reload the page and try again.");
        }
        if (!response.ok) throw new Error(payload.error || "Could not create prompt");

        bootstrapPrompt.value = payload.prompt;
        reveal(bootstrapPrompt, bootstrapResult, copyBootstrapStatus);
      } catch (error) {
        bootstrapFormStatus.textContent = error.message || "Could not create prompt";
      } finally {
        submit.disabled = false;
      }
    });
  }

  // Progressive enhancement: any server-rendered button carrying
  // data-copy-target="<field id>" copies that readonly field. The button is
  // rendered hidden so no-JS readers never see a dead control.
  for (const button of document.querySelectorAll("[data-copy-target]")) {
    const field = document.getElementById(button.dataset.copyTarget);
    if (!field) continue;
    const status = button.dataset.copyStatus ? document.getElementById(button.dataset.copyStatus) : null;
    button.hidden = false;
    button.addEventListener("click", async () => {
      await copyField(field, status, "Copied to clipboard", "Selected. Press Ctrl+C or Command+C to copy.");
    });
  }

  // A one-time secret revealed by a plain form POST (e.g. a new project's
  // RAISES_TOKEN) is selected so it can be copied immediately.
  const autofocus = document.querySelector("textarea[data-autofocus]");
  if (autofocus && autofocus.value) {
    autofocus.scrollIntoView({ block: "nearest" });
    autofocus.focus();
    autofocus.select();
  }

  // A plain form POST still renders the reveal server-side on a full
  // reload: bring a freshly revealed secret into view instead of
  // leaving the reader at the top of the page.
  for (const [field, resultPanel] of [[bootstrapPrompt, bootstrapResult], [token, panel]]) {
    if (field && resultPanel && !resultPanel.hidden && field.value) {
      resultPanel.scrollIntoView({ block: "nearest" });
      field.focus();
      break;
    }
  }
})();

(() => {
  const script = document.currentScript || document.querySelector("script[data-ga-id]");
  const id = script && script.dataset ? script.dataset.gaId : "";
  if (!id) return;
  window.dataLayer = window.dataLayer || [];
  function gtag() {
    window.dataLayer.push(arguments);
  }
  window.gtag = gtag;
  gtag("js", new Date());
  gtag("config", id);
})();

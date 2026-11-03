// Notices when the server now serves a different build than the one this page loaded, and offers
// a reload. It never reloads by itself: the page may hold unsaved work.
// The build is the list of scripts and stylesheets the entry document names; Vite hashes their
// file names, so a new release changes it. Dependency-free and CSP-safe: one same-origin GET of
// the entry document, parsed by the browser's inert DOMParser (no script runs, nothing loads),
// no inline script or style.

// The script and stylesheet URLs an HTML document names, in order. Parsed, not pattern-matched,
// so it reads the fetched page exactly as pageAssets reads the live one; comments are not elements.
export function buildAssets(html, parser = new DOMParser()) {
  return pageAssets(parser.parseFromString(html, 'text/html'));
}

// The same list from a live document. Read it at start-up, before code adds stylesheets.
export function pageAssets(doc = globalThis.document) {
  return [...doc.querySelectorAll('script[src], link[rel~="stylesheet"][href]')]
    .map(node => node.getAttribute(node.localName === 'script' ? 'src' : 'href'))
    .filter(Boolean);
}

// Checks on focus and when the tab becomes visible, and while visible on a timer, at most once
// per `interval`. Offline, a failed request, a redirect or a page without assets stays quiet.
// Shows the notice once; returns a function that stops checking and removes the notice.
export function watchForUpdate({ name = 'This app', url = '/', interval = 5 * 60_000, doc = globalThis.document, fetch = globalThis.fetch } = {}) {
  const view = doc.defaultView ?? globalThis;
  const loaded = pageAssets(doc).join('\n');
  let last = Date.now();
  let busy = false;
  let notice = null;

  const check = async () => {
    if (busy || notice || doc.visibilityState === 'hidden' || view.navigator?.onLine === false) return;
    if (Date.now() - last < interval) return;
    busy = true;
    last = Date.now();
    try {
      const response = await fetch(url, { cache: 'no-store', credentials: 'same-origin', redirect: 'manual', headers: { Accept: 'text/html' } });
      if (!response.ok || !/text\/html/i.test(response.headers.get('content-type') ?? '')) return;
      const served = buildAssets(await response.text(), new view.DOMParser());
      if (served.length && loaded && served.join('\n') !== loaded) show();
    } catch { /* Offline or the server is restarting: try again later. */ } finally { busy = false; }
  };

  const show = () => {
    stop();
    notice = doc.createElement('div');
    notice.className = 'ky-update';
    notice.setAttribute('role', 'status');
    const text = doc.createElement('p');
    text.className = 'ky-update-text';
    const reload = doc.createElement('button');
    reload.type = 'button';
    reload.className = 'ky-update-reload';
    reload.textContent = 'Reload';
    reload.addEventListener('click', () => view.location.reload());
    const dismiss = doc.createElement('button');
    dismiss.type = 'button';
    dismiss.className = 'ky-update-dismiss';
    dismiss.setAttribute('aria-label', 'Dismiss');
    dismiss.title = 'Dismiss';
    dismiss.textContent = '×';
    dismiss.addEventListener('click', () => { notice.remove(); });
    notice.append(text, reload, dismiss);
    doc.body.append(notice);
    // Filled after insertion so screen readers announce the polite status.
    text.textContent = `${name} was updated. Reload to use the new version.`;
  };

  const onVisible = () => { if (doc.visibilityState === 'visible') check(); };
  const timer = view.setInterval(check, interval);
  doc.addEventListener('visibilitychange', onVisible);
  view.addEventListener('focus', check);
  view.addEventListener('online', check);
  function stop() {
    view.clearInterval(timer);
    doc.removeEventListener('visibilitychange', onVisible);
    view.removeEventListener('focus', check);
    view.removeEventListener('online', check);
  }
  return () => { stop(); notice?.remove(); };
}

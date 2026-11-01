// A menu button (WAI-ARIA APG pattern) for the account footer's Settings gear: a button with
// aria-haspopup="menu" and aria-controls naming a role="menu" of link menuitems.
// Dependency-free; returns a function that removes every listener it added.
export function initMenuButton(button) {
  const doc = button.ownerDocument;
  const menu = doc.getElementById(button.getAttribute('aria-controls'));
  if (!menu) return () => {};
  const items = () => [...menu.querySelectorAll('[role="menuitem"]')];
  const isOpen = () => button.getAttribute('aria-expanded') === 'true';
  for (const item of items()) item.tabIndex = -1;
  button.setAttribute('aria-expanded', 'false');
  menu.hidden = true;

  const open = index => {
    button.setAttribute('aria-expanded', 'true');
    menu.hidden = false;
    items().at(index)?.focus();
  };
  const close = refocus => {
    if (!isOpen()) return;
    button.setAttribute('aria-expanded', 'false');
    menu.hidden = true;
    if (refocus) button.focus();
  };
  const move = (from, step) => {
    const list = items();
    list[(list.indexOf(from) + step + list.length) % list.length]?.focus();
  };

  // Enter and Space arrive as a click; a pointer click focuses the first item too, per the pattern.
  const onClick = () => (isOpen() ? close(false) : open(0));
  const onButtonKey = event => {
    if (event.key === 'Escape') close(true);
    if (event.key !== 'ArrowDown' && event.key !== 'ArrowUp') return;
    event.preventDefault();
    open(event.key === 'ArrowDown' ? 0 : -1);
  };
  const onMenuKey = event => {
    const item = event.target.closest('[role="menuitem"]');
    const list = items();
    if (event.key === 'ArrowDown') move(item, 1);
    else if (event.key === 'ArrowUp') move(item, -1);
    else if (event.key === 'Home') list[0]?.focus();
    else if (event.key === 'End') list.at(-1)?.focus();
    else if (event.key === 'Escape') close(true);
    else if (event.key === ' ') item?.click();
    else if (event.key === 'Tab') { close(false); return; }
    else if (event.key.length === 1 && !event.ctrlKey && !event.metaKey && !event.altKey) {
      // Type-ahead: the next item, after this one, whose label starts with the letter.
      const key = event.key.toLowerCase();
      const start = list.indexOf(item);
      const next = [...list.slice(start + 1), ...list.slice(0, start + 1)].find(el => el.textContent.trim().toLowerCase().startsWith(key));
      next?.focus();
    } else return;
    event.preventDefault();
  };
  // Choosing an item closes the menu; a same-page (hash or SPA) link leaves focus on the gear.
  const onMenuClick = event => { if (event.target.closest('[role="menuitem"]')) close(true); };
  const onPointerDown = event => {
    if (isOpen() && !button.contains(event.target) && !menu.contains(event.target)) close(false);
  };
  const onFocusOut = event => {
    const next = event.relatedTarget;
    if (next && !button.contains(next) && !menu.contains(next)) close(false);
  };

  button.addEventListener('click', onClick);
  button.addEventListener('keydown', onButtonKey);
  menu.addEventListener('keydown', onMenuKey);
  menu.addEventListener('click', onMenuClick);
  menu.addEventListener('focusout', onFocusOut);
  doc.addEventListener('pointerdown', onPointerDown);
  return () => {
    button.removeEventListener('click', onClick);
    button.removeEventListener('keydown', onButtonKey);
    menu.removeEventListener('keydown', onMenuKey);
    menu.removeEventListener('click', onMenuClick);
    menu.removeEventListener('focusout', onFocusOut);
    doc.removeEventListener('pointerdown', onPointerDown);
  };
}

// Wires every menu button under root, for server-rendered pages.
export function initMenuButtons(root = globalThis.document) {
  const cleanups = [...root.querySelectorAll('[aria-haspopup="menu"][aria-controls]')].map(initMenuButton);
  return () => cleanups.forEach(cleanup => cleanup());
}

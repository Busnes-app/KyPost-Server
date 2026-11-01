/** Wires one menu button (aria-haspopup="menu", aria-controls → role="menu"). Returns a cleanup. */
export function initMenuButton(button: HTMLElement): () => void;
/** Wires every `[aria-haspopup="menu"][aria-controls]` under root. Returns a cleanup. */
export function initMenuButtons(root?: ParentNode): () => void;

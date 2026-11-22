import { useRef, useState } from "react";
import type { KeyboardEvent } from "react";
import { Combobox } from "@base-ui/react/combobox";
import type { PGPRecipientTier } from "../api/pgp";
import { isPlausibleEmail } from "../lib/recipients";
import type { RecipientFieldState, RecipientToken } from "../lib/recipients";
import { useContactAutocomplete, type Suggestion } from "../hooks/useContactAutocomplete";
import { RecipientAutocomplete } from "./RecipientAutocomplete";

// recipientTierBadge maps a recipients-check tier to the short label shown
// next to a recipient's chip in compose. keyserver_confirm/key_changed are
// deliberately just a labeled warning here — the fingerprint-confirm modal
// that lets a user pin/accept those keys is a follow-up (see task-10-report).
function recipientTierBadge(tier: PGPRecipientTier | undefined): string | null {
  switch (tier) {
    case "verified":
      return "🔒 verified";
    case "wkd":
      return "🔒 found via WKD";
    case "keyserver_confirm":
      return "⚠️ confirm fingerprint";
    case "key_changed":
      return "⚠️ key changed";
    case "none":
      return "no key — pickup link";
    default:
      return null;
  }
}

/** A chip already in the field, or the suggestion just chosen. */
type Chosen = RecipientToken | Suggestion;

type RecipientFieldProps = {
  label: string; // "To", "Cc", "Bcc" — for placeholder/aria-label
  state: RecipientFieldState;
  onDraftChange: (draft: string) => void;
  onAddToken: (token: RecipientToken) => void;
  onRemoveToken: (index: number) => void;
  // Keyed by lowercased email address; populated from the recipients-check
  // response while encryption is enabled in compose. Omitted/empty when the
  // check hasn't run (e.g. encryption is off) — no badges render then.
  tiers?: Record<string, PGPRecipientTier>;
};

export function RecipientField({ label, state, onDraftChange, onAddToken, onRemoveToken, tiers }: RecipientFieldProps) {
  const [dismissed, setDismissed] = useState(false);
  const [fieldError, setFieldError] = useState("");
  const justHandledRef = useRef(false);
  // The option Combobox has highlighted; Tab accepts it, as Enter does.
  const highlightedRef = useRef<Suggestion | undefined>(undefined);
  const wrapRef = useRef<HTMLDivElement>(null);
  const fieldRef = useRef<HTMLDivElement>(null);

  const trimmedDraft = state.draft.trim();
  const { results, loading, searched } = useContactAutocomplete(state.draft, trimmedDraft.length > 0);
  const isOpen = !dismissed && (loading || searched);

  function selectContact(suggestion: Suggestion) {
    justHandledRef.current = true;
    onAddToken({ email: suggestion.email, name: suggestion.name || undefined, isCustom: false });
    onDraftChange("");
    setFieldError("");
    setDismissed(false);
  }

  function commitDraft() {
    const trimmed = state.draft.trim();
    if (!trimmed) return;
    if (isPlausibleEmail(trimmed)) {
      onAddToken({ email: trimmed, isCustom: true });
      onDraftChange("");
      setFieldError("");
      setDismissed(false);
    } else {
      setFieldError(`"${trimmed}" doesn't look like a valid email address.`);
    }
  }

  function handleDraftChange(draft: string) {
    justHandledRef.current = false;
    setFieldError("");
    setDismissed(false);
    onDraftChange(draft);
  }

  // The chips are the Combobox value. Options are Suggestions, never equal to
  // a chip, so choosing one always appends it (and useCompose refuses a
  // duplicate with its notice); a shorter value is a chip removed by its x,
  // Backspace or Delete.
  function handleValueChange(next: Chosen[]) {
    const added = next.find((v) => !state.tokens.includes(v as RecipientToken));
    if (added) {
      selectContact(added as Suggestion);
      return;
    }
    const removed = state.tokens.findIndex((token, i) => next[i] !== token);
    if (removed !== -1) onRemoveToken(removed);
  }

  function handleKeyDown(event: KeyboardEvent<HTMLInputElement> & { preventBaseUIHandler: () => void }) {
    if (event.key === "Tab") {
      if (isOpen && highlightedRef.current) {
        selectContact(highlightedRef.current);
      }
      return;
    }
    if (event.key === "Escape") {
      if (isOpen) {
        // Combobox closes the list; preventDefault keeps the compose <dialog>
        // from treating the same Escape as its own cancel.
        event.preventDefault();
      } else {
        // With the list closed, Combobox would clear every chip and the
        // draft. Escape belongs to the compose dialog then, as before.
        event.preventBaseUIHandler();
      }
      return;
    }
    if (event.key === "," || event.key === ";") {
      event.preventDefault();
      justHandledRef.current = true;
      commitDraft();
    }
  }

  function handleBlur() {
    if (justHandledRef.current) {
      justHandledRef.current = false;
      return;
    }
    commitDraft();
  }

  // Base UI Combobox (multiple): the input is a combobox, suggestions are
  // options in a listbox it names with aria-activedescendant, and the chips
  // are its value. Arrows move the highlight (stopping at the ends), Enter or
  // Tab accepts it, Escape closes the list, Backspace in an empty input or on
  // a chip removes one, and Left from the input start walks the chips. The
  // popup is portalled into this field so it stays inside the compose
  // <dialog>'s top layer.
  return (
    <Combobox.Root<Chosen, true>
      multiple
      items={results}
      filter={null}
      autoHighlight
      loopFocus={false}
      openOnInputClick={false}
      itemToStringLabel={(item) => item.email}
      value={state.tokens}
      onValueChange={(next, details) => {
        if (details.reason !== "escape-key") handleValueChange(next);
      }}
      inputValue={state.draft}
      onInputValueChange={(draft, details) => {
        // Only typing edits the draft. Combobox also clears the input when the
        // list closes or an option is chosen (input-clear) and on Escape;
        // the first would lose a half-typed address, and choosing clears it
        // in selectContact anyway.
        if (details.reason === "input-change") handleDraftChange(draft);
      }}
      open={isOpen}
      onOpenChange={(open) => setDismissed(!open)}
      onItemHighlighted={(item) => {
        highlightedRef.current = item as Suggestion | undefined;
      }}
    >
      <div className="compose-token-field-wrap" ref={wrapRef}>
        <Combobox.Chips className="compose-token-field" ref={fieldRef}>
          {state.tokens.map((token, index) => {
            const badge = recipientTierBadge(tiers?.[token.email.toLowerCase()]);
            return (
              <Combobox.Chip key={`${token.email}-${index}`} className="compose-token-pill" render={<span />}>
                <span className="compose-token-pill-label">{token.name ? `${token.name} <${token.email}>` : token.email}</span>
                {badge ? <span className="compose-token-pill-badge">{badge}</span> : null}
                {/* tabIndex 0: the x stays a Tab stop, as it was before chips could be reached with Left. */}
                <Combobox.ChipRemove className="compose-token-pill-remove" aria-label={`Remove ${token.email}`} tabIndex={0}>
                  &times;
                </Combobox.ChipRemove>
              </Combobox.Chip>
            );
          })}
          <Combobox.Input
            className="compose-token-input"
            placeholder={state.tokens.length === 0 ? `${label} recipients` : ""}
            aria-label={`${label} recipients`}
            onKeyDown={handleKeyDown}
            onBlur={handleBlur}
          />
        </Combobox.Chips>
        <Combobox.Portal container={wrapRef}>
          <Combobox.Positioner className="recipient-autocomplete-positioner" anchor={fieldRef} side="bottom" align="start" sideOffset={3} alignOffset={1}>
            <Combobox.Popup className="recipient-autocomplete">
              <RecipientAutocomplete query={state.draft} results={results} loading={loading} searched={searched} />
            </Combobox.Popup>
          </Combobox.Positioner>
        </Combobox.Portal>
        {fieldError ? <p className="compose-field-error">{fieldError}</p> : null}
      </div>
    </Combobox.Root>
  );
}

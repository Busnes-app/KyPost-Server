import { useEffect, useState, type ReactNode } from "react";
import { Combobox } from "@base-ui/react/combobox";
import { statusOf, statusText, type Presence } from "../api/kymessage";
import type { Suggestion } from "../hooks/useContactAutocomplete";

type RecipientAutocompleteProps = {
  query: string;
  results: Suggestion[];
  loading: boolean;
  searched: boolean;
};

function highlightMatch(text: string, query: string): ReactNode {
  if (!query) return text;
  const idx = text.toLowerCase().indexOf(query.toLowerCase());
  if (idx === -1) return text;
  const before = text.slice(0, idx);
  const match = text.slice(idx, idx + query.length);
  const after = text.slice(idx + query.length);
  return (
    <>
      {before}
      <strong className="recipient-autocomplete-match">{match}</strong>
      {after}
    </>
  );
}

/**
 * The suggestion list inside RecipientField's Combobox popup: the user's
 * contacts as options, then organisation people as options in one group
 * labelled "Organisation". Combobox owns highlight, pointer and keyboard.
 */
export function RecipientAutocomplete({ query, results, loading, searched }: RecipientAutocompleteProps) {
  // Organisation people's status from KyMessages, asked once per result list.
  const org = results.filter((r) => r.organisation).map((r) => r.email.toLowerCase()).join(",");
  const [status, setStatus] = useState<Record<string, Presence>>({});
  useEffect(() => {
    if (!org) return;
    let live = true;
    void statusOf(org.split(",")).then((s) => { if (live) setStatus(s); });
    return () => {
      live = false;
    };
  }, [org]);

  const showEmpty = searched && !loading && results.length === 0;

  const row = (item: Suggestion) => {
    const presence = item.organisation ? status[item.email.toLowerCase()] : undefined;
    return (
      <Combobox.Item key={item.key} value={item} className="recipient-autocomplete-row">
        <span className="recipient-autocomplete-name">{highlightMatch(item.name || item.email, query)}</span>
        {item.name ? <span className="recipient-autocomplete-email">{highlightMatch(item.email, query)}</span> : null}
        {presence ? (
          <span className="recipient-status" data-state={presence.state}>
            <span className="recipient-status-dot" aria-hidden="true" />
            {statusText(presence)}
          </span>
        ) : null}
      </Combobox.Item>
    );
  };
  const own = results.filter((r) => !r.organisation);
  const people = results.filter((r) => r.organisation);

  return (
    <>
      <Combobox.Empty>{showEmpty ? <div className="recipient-autocomplete-empty">No matches</div> : null}</Combobox.Empty>
      <Combobox.List className="recipient-autocomplete-list">
        {own.map(row)}
        {people.length > 0 ? (
          <Combobox.Group className="recipient-autocomplete-people">
            <Combobox.GroupLabel className="recipient-autocomplete-group">Organisation</Combobox.GroupLabel>
            {people.map(row)}
          </Combobox.Group>
        ) : null}
      </Combobox.List>
    </>
  );
}

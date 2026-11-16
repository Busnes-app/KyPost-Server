import { useEffect, useState, type ReactNode } from "react";
import { statusOf, statusText, type Presence } from "../api/kymessage";
import type { Suggestion } from "../hooks/useContactAutocomplete";

type RecipientAutocompleteProps = {
  query: string;
  results: Suggestion[];
  loading: boolean;
  searched: boolean;
  activeIndex: number;
  onHoverIndex: (i: number) => void;
  onSelect: (suggestion: Suggestion) => void;
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

export function RecipientAutocomplete({
  query,
  results,
  loading,
  searched,
  activeIndex,
  onHoverIndex,
  onSelect
}: RecipientAutocompleteProps) {
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

  if (!searched && !loading) {
    return null;
  }

  const showEmpty = searched && !loading && results.length === 0;

  return (
    <div className="recipient-autocomplete">
      {showEmpty ? (
        <div className="recipient-autocomplete-empty">No matches</div>
      ) : (
        results.map((item, index) => (
          <div key={item.key} role="presentation">
            {item.organisation && !results[index - 1]?.organisation ? (
              <div className="recipient-autocomplete-group">Organisation</div>
            ) : null}
            <div
              className={`recipient-autocomplete-row${index === activeIndex ? " active" : ""}`}
              onMouseEnter={() => onHoverIndex(index)}
              onMouseDown={(e) => e.preventDefault()}
              onClick={() => onSelect(item)}
            >
              <span className="recipient-autocomplete-name">{highlightMatch(item.name || item.email, query)}</span>
              {item.name ? <span className="recipient-autocomplete-email">{highlightMatch(item.email, query)}</span> : null}
              {item.organisation && status[item.email.toLowerCase()] ? (
                <span className="recipient-status" data-state={status[item.email.toLowerCase()].state}>
                  <span className="recipient-status-dot" aria-hidden="true" />
                  {statusText(status[item.email.toLowerCase()])}
                </span>
              ) : null}
            </div>
          </div>
        ))
      )}
    </div>
  );
}

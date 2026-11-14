import { useEffect, useState } from "react";
import { getJSON } from "../api/client";
import { searchContacts } from "../api/contacts";

/** One autocomplete row: the user's own contact, or a person in the organisation. */
export type Suggestion = { key: string; name: string; email: string; organisation: boolean };

/** People in the organisation (GET /api/suite/people); any failure is none. */
async function searchOrganisation(q: string): Promise<Suggestion[]> {
  if (q.length < 2) return [];
  try {
    const { people } = await getJSON<{ people?: unknown }>(`/api/suite/people?q=${encodeURIComponent(q)}`);
    if (!Array.isArray(people)) return [];
    return people
      .filter((p): p is { name?: unknown; email: string } => typeof p?.email === "string" && p.email.includes("@"))
      .map((p) => ({ key: `org:${p.email}`, name: typeof p.name === "string" ? p.name : "", email: p.email, organisation: true }));
  } catch {
    return [];
  }
}

/** Contacts first, then organisation people under their own heading, deduplicated by address. */
export function useContactAutocomplete(query: string, enabled: boolean) {
  const [results, setResults] = useState<Suggestion[]>([]);
  const [loading, setLoading] = useState(false);
  const [searched, setSearched] = useState(false);

  useEffect(() => {
    const trimmed = query.trim();
    if (!enabled || !trimmed) {
      setResults([]);
      setLoading(false);
      setSearched(false);
      return;
    }
    let cancelled = false;
    setLoading(true);
    const timeoutId = setTimeout(() => {
      const contacts = searchContacts(trimmed, 5)
        .then(({ contacts }) => contacts.flatMap((c) => {
          const email = c.emails?.[0]?.value ?? "";
          return email ? [{ key: c.uid, name: c.fn, email, organisation: false }] : [];
        }))
        .catch(() => [] as Suggestion[]);
      void Promise.all([contacts, searchOrganisation(trimmed)]).then(([own, org]) => {
        if (cancelled) return;
        const seen = new Set(own.map((s) => s.email.toLowerCase()));
        setResults([...own, ...org.filter((s) => !seen.has(s.email.toLowerCase()) && seen.add(s.email.toLowerCase()))]);
        setSearched(true);
        setLoading(false);
      });
    }, 150);
    return () => {
      cancelled = true;
      clearTimeout(timeoutId);
    };
  }, [query, enabled]);

  return { results, loading, searched };
}

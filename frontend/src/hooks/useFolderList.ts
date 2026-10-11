import { useRef, useState } from "react";
import { getJSON, selectedMailboxId } from "../api/client";
import type { InboxFolder, InboxFoldersResponse } from "../app/types";

// One sidebar folder list. Only the newest load, for the mailbox still
// selected, may write: a slow refresh must not overwrite a newer one.
export function useFolderList(url: string) {
  const [folders, setFolders] = useState<InboxFolder[]>([]);
  const [loading, setLoading] = useState(false);
  const seq = useRef(0);

  async function load() {
    const id = ++seq.current;
    const box = selectedMailboxId();
    const current = () => id === seq.current && box === selectedMailboxId();
    setLoading(true);
    try {
      const data = await getJSON<InboxFoldersResponse>(url);
      if (current()) setFolders(data.folders ?? []);
    } catch {
      if (current()) setFolders([]);
    } finally {
      if (current()) setLoading(false);
    }
  }

  function reset() {
    seq.current++;
    setFolders([]);
    setLoading(false);
  }

  return { folders, loading, load, reset };
}

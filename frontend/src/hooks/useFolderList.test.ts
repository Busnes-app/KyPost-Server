import { describe, expect, it, vi } from "vitest";
import { act, renderHook } from "@testing-library/react";

const getJSON = vi.fn();
vi.mock("../api/client", () => ({ getJSON: (url: string) => getJSON(url), selectedMailboxId: () => "" }));

import { useFolderList } from "./useFolderList";

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((r) => (resolve = r));
  return { promise, resolve };
}

describe("useFolderList", () => {
  it("keeps the newest load when an older one answers last", async () => {
    const stale = deferred<unknown>();
    const fresh = deferred<unknown>();
    getJSON.mockReturnValueOnce(stale.promise).mockReturnValueOnce(fresh.promise);
    const { result } = renderHook(() => useFolderList("/api/inbox/folders"));

    let first!: Promise<void>, second!: Promise<void>;
    act(() => {
      first = result.current.load();
    });
    act(() => {
      second = result.current.load();
    });
    await act(async () => {
      fresh.resolve({ folders: [{ path: "INBOX/New", deletable: true }] });
      await second;
      stale.resolve({ folders: [] });
      await first;
    });

    expect(result.current.folders.map((f) => f.path)).toEqual(["INBOX/New"]);
    expect(result.current.loading).toBe(false);
  });

  it("drops a load that answers after reset", async () => {
    const late = deferred<unknown>();
    getJSON.mockReturnValueOnce(late.promise);
    const { result } = renderHook(() => useFolderList("/api/inbox/folders"));

    let pending!: Promise<void>;
    act(() => {
      pending = result.current.load();
    });
    act(() => result.current.reset());
    await act(async () => {
      late.resolve({ folders: [{ path: "Old", deletable: true }] });
      await pending;
    });

    expect(result.current.folders).toEqual([]);
  });
});

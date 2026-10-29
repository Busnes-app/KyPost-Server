/**
 * Test double for a SQLite-backed Durable Object's `ctx.storage`: the KV-style
 * get/put/delete the token and budget instances use, over a Map, and `sql` /
 * `transactionSync` over a real in-memory SQLite, so the key registry's SQL runs
 * as written rather than against a mock of it.
 *
 * Not a test file (no `.test.`), so the runners load it only through an import.
 */

import { DatabaseSync } from "node:sqlite";

export function durableStorage() {
  const cells = new Map();
  const db = new DatabaseSync(":memory:");
  return {
    cells,
    db,
    async get(key) {
      return cells.get(key);
    },
    async put(keyOrEntries, value) {
      if (typeof keyOrEntries === "string") {
        cells.set(keyOrEntries, value);
        return;
      }
      for (const [k, v] of Object.entries(keyOrEntries)) {
        cells.set(k, v);
      }
    },
    async delete(keys) {
      for (const key of Array.isArray(keys) ? keys : [keys]) {
        cells.delete(key);
      }
    },
    sql: {
      exec(query, ...bindings) {
        const rows = db
          .prepare(query)
          .all(...bindings)
          .map((row) => ({ ...row }));
        return { toArray: () => rows, one: () => rows[0], [Symbol.iterator]: () => rows[Symbol.iterator]() };
      },
    },
    transactionSync(closure) {
      db.exec("SAVEPOINT tx");
      try {
        const result = closure();
        db.exec("RELEASE tx");
        return result;
      } catch (err) {
        db.exec("ROLLBACK TO tx");
        db.exec("RELEASE tx");
        throw err;
      }
    },
  };
}

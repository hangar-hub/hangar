// Checks that the parts upload.js sends hash to the SHA-256 it plans with.
// Run with: node --test loadtest/lib/
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { test } from "node:test";

import { CHUNK, eachChunk, newBase, part } from "./payload.js";

function planned(vu, iter, size) {
  const h = createHash("sha256");
  eachChunk(newBase(vu), vu, iter, size, (c) => h.update(c));
  return h.digest("hex");
}

function sent(vu, iter, size, partSize) {
  // One buffer for both passes, as upload.js uses: the hash pass leaves
  // the last chunk's header in it before the first part is built.
  const base = newBase(vu);
  eachChunk(base, vu, iter, size, () => {});
  const h = createHash("sha256");
  for (let n = 1; (n - 1) * partSize < size; n++) h.update(new Uint8Array(part(base, vu, iter, n, partSize, size)));
  return h.digest("hex");
}

test("the parts hash to the planned SHA-256", () => {
  // 12 chunks and a short tail: the chunk index goes from one digit to two.
  const size = 12 * CHUNK + 12345;
  for (const [vu, iter, partSize] of [[1, 0, 4 * CHUNK], [3, 7, 5 * CHUNK], [12, 10, CHUNK]]) {
    assert.equal(sent(vu, iter, size, partSize), planned(vu, iter, size), `vu ${vu} iter ${iter} part size ${partSize}`);
  }
});

test("objects of different VUs and iterations differ", () => {
  const size = 2 * CHUNK;
  assert.notEqual(planned(1, 0, size), planned(2, 0, size));
  assert.notEqual(planned(1, 0, size), planned(1, 1, size));
});

test("a part size that is not a multiple of a chunk is refused", () => {
  assert.throws(() => part(newBase(1), 1, 0, 1, CHUNK + 1, 4 * CHUNK), /not a multiple/);
});

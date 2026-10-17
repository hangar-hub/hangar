// The bytes upload.js sends: a deterministic object per run, VU and
// iteration, built 1 MiB chunk at a time from one reused buffer, so a 1 GiB
// object never sits in memory whole. Every chunk starts with a header naming
// the run, VU, iteration and chunk, so nothing is deduplicated, within a run
// or against an object an earlier run stored.
//
// This module uses no k6 APIs, so loadtest/lib/payload.test.js checks it
// with Node.

export const CHUNK = 1 << 20;

// Every header has the same length, so writing one over the reused buffer
// replaces the whole previous header and a chunk's bytes depend only on
// (run, vu, iter, index), not on which chunk was built before it.
const WIDTH = 16;

function pad(n) {
  return String(n).padStart(WIDTH, "0");
}

// newBase returns the reused chunk buffer for a VU.
export function newBase(vu) {
  const base = new Uint8Array(CHUNK);
  for (let i = 0; i < CHUNK; i++) base[i] = (i * 31 + vu) & 0xff;
  return base;
}

// chunk writes the header of chunk index i into base and returns base.
export function chunk(base, run, vu, iter, i) {
  const header = `run=${pad(run)} vu=${pad(vu)} iter=${pad(iter)} chunk=${pad(i)} `;
  for (let k = 0; k < header.length; k++) base[k] = header.charCodeAt(k);
  return base;
}

// eachChunk calls fn with every chunk of an object of size bytes, in
// order, trimmed to the object's end.
export function eachChunk(base, run, vu, iter, size, fn) {
  for (let off = 0; off < size; off += CHUNK) {
    fn(chunk(base, run, vu, iter, off / CHUNK).subarray(0, Math.min(CHUNK, size - off)));
  }
}

// part returns part n (1-based) of the object as an ArrayBuffer. partSize
// must be a multiple of CHUNK, as Hangar's part sizes are.
export function part(base, run, vu, iter, n, partSize, size) {
  if (partSize % CHUNK !== 0) throw new Error(`part size ${partSize} is not a multiple of ${CHUNK}`);
  const off = (n - 1) * partSize;
  const len = Math.min(partSize, size - off);
  const out = new Uint8Array(len);
  for (let o = 0; o < len; o += CHUNK) {
    const c = chunk(base, run, vu, iter, (off + o) / CHUNK);
    out.set(c.subarray(0, Math.min(CHUNK, len - o)), o);
  }
  return out.buffer;
}

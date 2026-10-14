import { sha256 } from "@noble/hashes/sha2.js";
import { bytesToHex } from "@noble/hashes/utils.js";

/**
 * Hashes a stream with SHA-256 incrementally, so a file of any size is
 * read once in chunks and never held in memory whole.
 */
export async function sha256Stream(
  stream: ReadableStream<Uint8Array>,
  onProgress?: (bytesDone: number) => void,
): Promise<string> {
  const h = sha256.create();
  const reader = stream.getReader();
  let done = 0;
  let last = 0;
  for (;;) {
    const { value, done: end } = await reader.read();
    if (end) break;
    h.update(value);
    done += value.length;
    if (onProgress && done - last >= 4 * 1024 * 1024) {
      onProgress(done);
      last = done;
    }
  }
  onProgress?.(done);
  return bytesToHex(h.digest());
}

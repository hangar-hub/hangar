/// <reference lib="webworker" />
import { sha256Stream } from "./sha256";

/** Hashes one File off the main thread and reports progress (09-web-ui.md "Uploads in the browser"). */
self.onmessage = async (e: MessageEvent<{ file: File }>) => {
  try {
    const hex = await sha256Stream(e.data.file.stream(), (done) =>
      self.postMessage({ type: "progress", done }),
    );
    self.postMessage({ type: "done", hex });
  } catch (err) {
    self.postMessage({ type: "error", message: err instanceof Error ? err.message : String(err) });
  }
};

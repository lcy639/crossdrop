import { createSHA256 } from "hash-wasm";

self.onmessage = async (event: MessageEvent<File>) => {
  try {
    const file = event.data;
    const hash = await createSHA256();
    hash.init();
    const block = 8 * 1024 * 1024;
    for (let start = 0; start < file.size; start += block) {
      hash.update(
        new Uint8Array(await file.slice(start, start + block).arrayBuffer()),
      );
      self.postMessage({ progress: Math.min(1, (start + block) / file.size) });
    }
    self.postMessage({ hash: hash.digest("hex") });
  } catch (error) {
    self.postMessage({ error: String(error) });
  }
};

import { expect } from "@playwright/test";
import { openPage } from "./fixtures.mjs";

// A silent WAV lets the real media element play, seek, pause and end offline.
const sampleRate = 8000;
const audio = Buffer.alloc(44 + sampleRate * 2 * 120);
audio.write("RIFF", 0);
audio.writeUInt32LE(audio.length - 8, 4);
audio.write("WAVEfmt ", 8);
audio.writeUInt32LE(16, 16);
audio.writeUInt16LE(1, 20);
audio.writeUInt16LE(1, 22);
audio.writeUInt32LE(sampleRate, 24);
audio.writeUInt32LE(sampleRate * 2, 28);
audio.writeUInt16LE(2, 32);
audio.writeUInt16LE(16, 34);
audio.write("data", 36);
audio.writeUInt32LE(audio.length - 44, 40);

export async function openPlayer(page, { source = "HLS", tracks = 1, language = "en", subtitles = [], files } = {}) {
  await page.addInitScript((source) => {
    // Tests start playback explicitly so seek assertions do not race autoplay.
    document.addEventListener("DOMContentLoaded", () => {
      document.querySelector("#video").autoplay = false;
    });
    if (source === "native") {
      const canPlayType = HTMLMediaElement.prototype.canPlayType;
      HTMLMediaElement.prototype.canPlayType = function (type) {
        return type === "application/vnd.apple.mpegurl" ? "probably" : canPlayType.call(this, type);
      };
      return;
    }
    // Replace only the HLS transport; playback and media events remain real.
    window.Hls = class {
      static Events = { MANIFEST_PARSED: "manifest", FRAG_LOADED: "fragment", ERROR: "error" };
      static isSupported() {
        return true;
      }
      constructor(options) {
        window.testHls = this;
        this.options = options;
        this.handlers = new Map();
      }
      on(event, handler) {
        this.handlers.set(event, handler);
      }
      loadSource(src) {
        this.src = src;
      }
      attachMedia(video) {
        video.src = this.src;
        video.addEventListener("loadedmetadata", () => this.handlers.get("manifest")?.(), {
          once: true,
        });
      }
      destroy() {}
    };
  }, source);
  await openPage(page, "/", language);
  const download = {
    id: "ready",
    title: "Two episodes",
    magnet: "magnet:?xt=urn:btih:0123456789012345678901234567890123456789",
    status: "ready",
    size: 1024,
    expiresAt: "2099-01-01T00:00:00Z",
    selectedFileIndex: 4,
    files: files ?? [
      { index: 2, name: "Episode 1.mkv", size: 512 },
      { index: 4, name: "Episode 2.mkv", size: 512 },
    ],
  };
  await page.route(/\/api\/downloads(?:\?|$)/, (route) =>
    route.fulfill({
      json: [download, { ...download, id: "other", title: "Other video", magnet: "magnet:other" }],
    }),
  );
  await page.route("**/api/hls/prepare?*", (route) => {
    if (route.request().method() === "POST") {
      const query = new URL(route.request().url()).searchParams;
      if (query.get("magnet") === download.magnet) download.selectedFileIndex = Number(query.get("file"));
      return route.fulfill({ status: 204 });
    }
    const range = route
      .request()
      .headers()
      .range?.match(/^bytes=(\d+)-(\d*)$/);
    const start = range ? Number(range[1]) : 0;
    const end = range?.[2] ? Number(range[2]) : audio.length - 1;
    return route.fulfill({
      status: range ? 206 : 200,
      contentType: "audio/wav",
      headers: {
        "Accept-Ranges": "bytes",
        ...(range ? { "Content-Range": `bytes ${start}-${end}/${audio.length}` } : {}),
      },
      body: audio.subarray(start, end + 1),
    });
  });
  await page.route("**/api/media/info?*", (route) =>
    route.fulfill({
      json: {
        audioTracks: Array.from({ length: tracks }, (_, index) => ({ index, language: index ? "eng" : "rus", codec: "aac" })),
        subtitles,
      },
    }),
  );
  await page.evaluate(() => window.dispatchEvent(new Event("focus")));
  await expect(page.locator('.download-row[data-id="ready"] .download-title')).toHaveText(
    download.title,
  );
  return page.locator('.download-row[data-id="ready"]');
}

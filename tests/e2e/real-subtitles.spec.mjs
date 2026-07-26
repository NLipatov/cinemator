import { expect, test } from '@playwright/test';
import { execFileSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';

const hlsSource = readFileSync(resolve('node_modules/hls.js/dist/hls.min.js'));

let fixtureDir;
let videoSegments;

test.beforeAll(async () => {
  fixtureDir = await mkdtemp(join(tmpdir(), 'cinemator-real-subtitles-'));
  execFileSync('ffmpeg', [
    '-hide_banner', '-loglevel', 'error', '-y',
    '-f', 'lavfi', '-i', 'color=c=black:s=320x180:r=24:d=4',
    '-f', 'lavfi', '-i', 'anullsrc=channel_layout=stereo:sample_rate=48000',
    '-t', '4',
    '-c:v', 'libx264', '-preset', 'ultrafast', '-pix_fmt', 'yuv420p',
    '-c:a', 'aac',
    '-force_key_frames', 'expr:gte(t,n_forced*2)',
    '-f', 'hls',
    '-hls_time', '2',
    '-hls_list_size', '0',
    '-hls_segment_filename', join(fixtureDir, 'chunk_%06d.ts'),
    join(fixtureDir, 'index.m3u8'),
  ]);
  videoSegments = [
    await readFile(join(fixtureDir, 'chunk_000000.ts')),
    await readFile(join(fixtureDir, 'chunk_000001.ts')),
  ];
});

test.afterAll(async () => {
  if (fixtureDir) await rm(fixtureDir, { recursive: true, force: true });
});

test('renders a later selected cue after an empty leading segment through real hls.js', async ({ page }) => {
  const requests = [];
  page.on('request', request => requests.push(new URL(request.url()).pathname));

  await page.route('https://cdn.jsdelivr.net/npm/hls.js@1.6.13', route => route.fulfill({
    contentType: 'text/javascript',
    body: hlsSource,
  }));
  await page.route('**/api/**', async route => {
    const url = new URL(route.request().url());
    if (url.pathname === '/api/auth/status') {
      await route.fulfill({ json: { enabled: false, version: 'test-build' } });
      return;
    }
    if (url.pathname === '/api/downloads') {
      await route.fulfill({ json: [] });
      return;
    }
    if (url.pathname === '/api/torrent/files') {
      await route.fulfill({
        json: [{ index: 0, name: 'movie.mkv', size: videoSegments.reduce((sum, segment) => sum + segment.length, 0) }],
      });
      return;
    }
    if (url.pathname === '/api/media/info') {
      await route.fulfill({
        json: {
          duration: 4,
          seekable: true,
          videoCodec: 'h264',
          videoCodecString: 'avc1.64001e',
          width: 320,
          height: 180,
          frameRate: 24,
          bitrate: 1_000_000,
          audioTracks: [{ index: 0, codec: 'aac', channels: 2 }],
          subtitles: [{ index: 0, codec: 'subrip', language: 'eng' }],
        },
      });
      return;
    }
    if (url.pathname === '/api/hls/prepare') {
      await route.fulfill({
        json: {
          playlist: '/api/hls/real-subtitles/master.m3u8',
          stream: 'real-subtitles',
          segmentDurationSeconds: 2,
          windowSegments: 2,
        },
      });
      return;
    }
    if (url.pathname === '/api/hls/status/real-subtitles') {
      await route.fulfill({
        json: {
          phase: 'ready',
          stage: 'ready',
          targetSeconds: 0,
          presentationOriginSeconds: 0,
          generation: 'fixture',
          activePeers: 1,
          totalPeers: 1,
        },
      });
      return;
    }
    if (url.pathname === '/api/hls/real-subtitles/master.m3u8') {
      await route.fulfill({
        contentType: 'application/vnd.apple.mpegurl',
        body: [
          '#EXTM3U',
          '#EXT-X-VERSION:3',
          '#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",NAME="Subtitles",DEFAULT=YES,AUTOSELECT=YES,FORCED=NO,URI="subs.m3u8?v=fixture",LANGUAGE="eng"',
          '#EXT-X-STREAM-INF:BANDWIDTH=1000000,CODECS="avc1.64001e,mp4a.40.2",SUBTITLES="subs"',
          'index.m3u8?v=fixture',
          '',
        ].join('\n'),
      });
      return;
    }
    if (url.pathname === '/api/hls/real-subtitles/index.m3u8') {
      await route.fulfill({
        contentType: 'application/vnd.apple.mpegurl',
        body: [
          '#EXTM3U',
          '#EXT-X-VERSION:3',
          '#EXT-X-TARGETDURATION:2',
          '#EXT-X-MEDIA-SEQUENCE:0',
          '#EXTINF:2.000,',
          'chunk_000000.ts?v=fixture',
          '#EXTINF:2.000,',
          'chunk_000001.ts?v=fixture',
          '#EXT-X-ENDLIST',
          '',
        ].join('\n'),
      });
      return;
    }
    if (url.pathname === '/api/hls/real-subtitles/subs.m3u8') {
      await route.fulfill({
        contentType: 'application/vnd.apple.mpegurl',
        body: [
          '#EXTM3U',
          '#EXT-X-VERSION:3',
          '#EXT-X-TARGETDURATION:2',
          '#EXT-X-MEDIA-SEQUENCE:0',
          '#EXTINF:2.000,',
          'subs_000000.vtt?v=fixture',
          '#EXTINF:2.000,',
          'subs_000001.vtt?v=fixture',
          '#EXT-X-ENDLIST',
          '',
        ].join('\n'),
      });
      return;
    }
    if (url.pathname === '/api/hls/real-subtitles/chunk_000000.ts') {
      await route.fulfill({ contentType: 'video/mp2t', body: videoSegments[0] });
      return;
    }
    if (url.pathname === '/api/hls/real-subtitles/chunk_000001.ts') {
      await route.fulfill({ contentType: 'video/mp2t', body: videoSegments[1] });
      return;
    }
    if (url.pathname === '/api/hls/real-subtitles/subs_000000.vtt') {
      await route.fulfill({
        contentType: 'text/vtt; charset=utf-8',
        body: [
          'WEBVTT',
          'X-TIMESTAMP-MAP=LOCAL:00:00:00.000,MPEGTS:0',
          '',
        ].join('\n'),
      });
      return;
    }
    if (url.pathname === '/api/hls/real-subtitles/subs_000001.vtt') {
      await route.fulfill({
        contentType: 'text/vtt; charset=utf-8',
        body: [
          'WEBVTT',
          'X-TIMESTAMP-MAP=LOCAL:00:00:02.000,MPEGTS:180000',
          '',
          '00:00:02.200 --> 00:00:03.800',
          'real subtitle fixture',
          '',
        ].join('\n'),
      });
      return;
    }
    await route.fulfill({ status: 404 });
  });

  await page.goto('/');
  await page.getByLabel('Magnet link').fill('magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567');
  await page.getByRole('button', { name: 'Load', exact: true }).click();
  await page.getByRole('button', { name: 'Select tracks' }).click();
  await page.locator('#subtitleSelect').selectOption('0');
  await page.getByRole('button', { name: 'Play', exact: true }).click();

  const video = page.locator('video');
  await expect.poll(() => video.evaluate(element => element.textTracks.length)).toBe(1);
  await expect.poll(() => video.evaluate(element => element.textTracks[0].mode)).toBe('showing');
  await expect.poll(() => video.evaluate(element =>
    Array.from(element.textTracks[0].cues || [], cue => cue.text),
  )).toContain('real subtitle fixture');
  expect(requests).toContain('/api/hls/real-subtitles/subs_000000.vtt');
  expect(requests).toContain('/api/hls/real-subtitles/subs_000001.vtt');
});

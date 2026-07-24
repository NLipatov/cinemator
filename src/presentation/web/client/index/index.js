    const themeKey = 'theme-mode';
    const themes = ['dark', 'light'];
    const $toggle = document.getElementById('themeToggle');
    const $moon = document.getElementById('icon-moon');
    const $sun  = document.getElementById('icon-sun');
    let themeIdx = 0;
    function setTheme(idx, save=true) {
      document.documentElement.setAttribute('data-theme', themes[idx]);
      $moon.style.display = (idx === 0) ? '' : 'none';
      $sun.style.display  = (idx === 1) ? '' : 'none';
      themeIdx = idx;
      if (save) localStorage.setItem(themeKey, themes[idx]);
    }
    $toggle.onclick = function() { setTheme(1-themeIdx); };
    (function() {
      let mode = localStorage.getItem(themeKey);
      if (!themes.includes(mode)) {
        mode = window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
      }
      setTheme(themes.indexOf(mode), false);
    })();

    // Main logic
    const $ = id => document.getElementById(id);
    async function apiFetch(input, init = {}, timeoutMs = 0) {
      let timer = null;
      let timedOut = false;
      let abortFromCaller = null;
      const callerSignal = init.signal;
      const controller = timeoutMs > 0 ? new AbortController() : null;
      if (controller && callerSignal) {
        if (callerSignal.aborted) {
          controller.abort();
        } else {
          abortFromCaller = () => controller.abort();
          callerSignal.addEventListener('abort', abortFromCaller, { once: true });
        }
      }
      if (controller) {
        timer = setTimeout(() => {
          timedOut = true;
          controller.abort();
        }, timeoutMs);
        init = { ...init, signal: controller.signal };
      }
      try {
        const response = await window.fetch(input, init);
        if (response.status === 401) {
          window.location.replace('/login');
          throw new Error('Session expired');
        }
        return response;
      } catch (error) {
        if (timedOut) {
          throw new Error(`Server did not respond within ${Math.ceil(timeoutMs / 1000)} seconds`);
        }
        throw error;
      } finally {
        if (timer) clearTimeout(timer);
        if (abortFromCaller && callerSignal) {
          callerSignal.removeEventListener('abort', abortFromCaller);
        }
      }
    }

    const logoutBtn = $('logoutBtn');
    const appVersion = $('appVersion');
    if (logoutBtn) {
      window.fetch('/api/auth/status', { cache: 'no-store' })
        .then(response => response.ok ? response.json() : null)
        .then(status => {
          logoutBtn.hidden = !status?.enabled;
          if (status?.version) appVersion.textContent = status.version;
        })
        .catch(() => {});
      logoutBtn.addEventListener('click', async () => {
        logoutBtn.disabled = true;
        try {
          const response = await apiFetch('/api/auth/logout', { method: 'POST' });
          if (!response.ok) throw new Error('Could not sign out');
          window.location.replace('/login');
        } catch (error) {
          if (window.location.pathname !== '/login') logoutBtn.disabled = false;
        }
      });
    }

    let subtitleDelay = parseFloat(localStorage.getItem('subtitle-delay') || '0');
    let msgTimeout = null;
    let subtitleWaitTimer = null;
    let downloadCatalog = [];
    let downloadsLoading = false;
    let downloadsRefreshQueued = false;
    let downloadEvents = null;
    let downloadFallbackPolling = false;
    let downloadPollingTimer = null;
    let openExtendDownloadID = null;
    function mediaBufferedRanges(video) {
      const ranges = [];
      for (let index = 0; index < (video?.buffered?.length || 0); index++) {
        ranges.push({ start: video.buffered.start(index), end: video.buffered.end(index) });
      }
      return ranges;
    }
    class PlaybackTimeline {
      constructor() { this.reset(); }
      reset() {
        this.sourceStart = 0;
        this.presentationOrigin = 0;
        this.mediaAnchor = null;
        this.absolute = false;
      }
      configure({ sourceStart, duration, presentationOrigin, seekable }) {
        this.sourceStart = Math.max(0, Number(sourceStart) || 0);
        this.presentationOrigin = Math.max(0, Number(presentationOrigin) || 0);
        this.mediaAnchor = null;
        this.absolute = Boolean(seekable && Number.isFinite(duration) && duration > 0);
        return this.absolute ? this.presentationOrigin : undefined;
      }
      hlsTime(sourceTime) {
        return this.absolute ? Math.max(0, sourceTime - this.presentationOrigin) : sourceTime;
      }
      sourceTime(currentTime) {
        if (currentTime === undefined) return this.sourceStart;
        const local = Number.isFinite(currentTime) ? currentTime : this.mediaAnchor;
        if (this.absolute && Number.isFinite(local)) {
          return Math.max(0, local);
        }
        if (!Number.isFinite(this.mediaAnchor)) return this.sourceStart;
        return Math.max(0, this.sourceStart + local - this.mediaAnchor);
      }
      anchor(currentTime) {
        if (!Number.isFinite(this.mediaAnchor) && Number.isFinite(currentTime)) {
          this.mediaAnchor = currentTime;
        }
      }
      buffered(ranges, time) {
        for (const range of ranges) {
          if (time >= range.start - 0.05 && time < range.end + 0.05) return true;
        }
        return false;
      }
      contains(time, ranges, fragments) {
        if (!this.absolute) return true;
        if (this.buffered(ranges, time)) return true;
        if (!fragments?.length) return false;
        const first = fragments[0];
        const last = fragments[fragments.length - 1];
        return time >= first.start - 0.25 && time < last.start + last.duration + 0.25;
      }
    }
    const decodedSeekFrameToleranceSeconds = 0.5;
    class PlaybackSession {
      constructor() {
        this.timeline = new PlaybackTimeline();
        this.hls = null;
        this.events = null;
        this.requestSeq = 0;
        this.requestController = null;
        this.requestTarget = null;
        this.seekCommitTimer = null;
        this.seekCommitTarget = null;
        this.streamDir = null;
        this.generation = '';
        this.attaching = false;
        this.protectedMediaTime = null;
        this.committedSeekMediaTime = null;
        this.restoringPlayhead = false;
        this.mediaSeekable = true;
        this.mediaInfo = null;
        this.mode = '';
        this.decision = '';
        this.usesPassthrough = false;
        this.forceTranscode = false;
        this.nativeRetries = 0;
        this.notice = '';
        this.lastActivityAt = 0;
        this.statusTimer = null;
        this.statusController = null;
        this.statusSeq = 0;
        this.statusTarget = null;
        this.missingStreamRecoveryAttempted = false;
        this.presentationRecoveryAttempted = false;
        this.frameCallback = null;
        this.frameTimer = null;
        this.lastPresentedFrameAt = 0;
        this.hasPresentedFrame = false;
        this.playIntent = false;
        this.autoplayBlocked = false;
        this.presentationAttemptSeq = 0;
        this.presentationAttempt = null;
        this.stallStartedAt = 0;
        this.stallWasUnderrun = false;
        this.presentedFrameCount = 0;
        this.qoe = {
          attempts: [],
          playbackStallCount: 0,
          playbackStallDurationMs: 0,
          rebufferCount: 0,
          rebufferDurationMs: 0,
          intendedPlayingMs: 0,
          intendedPlayingStartedAt: 0,
          stalls: [],
          lastPublishedAt: 0,
          publishTimer: null,
        };
      }
      isStale(id) { return id !== this.requestSeq; }
      cancelRequest() {
        this.requestSeq++;
        if (this.requestController) this.requestController.abort();
        this.requestController = null;
        this.requestTarget = null;
      }
      beginRequest(target = null) {
        this.cancelRequest();
        const request = { id: this.requestSeq, controller: new AbortController() };
        request.signal = request.controller.signal;
        this.requestController = request.controller;
        this.requestTarget = Number.isFinite(target) ? target : null;
        return request;
      }
      isRequesting(target) {
        return this.requestController && Number.isFinite(this.requestTarget) &&
          Math.abs(this.requestTarget - target) < 0.25;
      }
      finishRequest(request) {
        if (this.requestController === request.controller) {
          this.requestController = null;
          this.requestTarget = null;
        }
      }
      cancelSeekCommit() {
        if (this.seekCommitTimer) clearTimeout(this.seekCommitTimer);
        this.seekCommitTimer = null;
        this.seekCommitTarget = null;
      }
      scheduleSeekCommit(target, commit) {
        this.cancelSeekCommit();
        this.seekCommitTarget = target;
        this.seekCommitTimer = setTimeout(() => {
          this.seekCommitTimer = null;
          this.seekCommitTarget = null;
          commit(target);
        }, seekCommitDelayMs);
      }
      commitSeekPosition(mediaTime) {
        if (Number.isFinite(mediaTime)) this.committedSeekMediaTime = mediaTime;
      }
      restorationMediaTime() {
        return Number.isFinite(this.committedSeekMediaTime)
          ? this.committedSeekMediaTime
          : this.protectedMediaTime;
      }
      acceptPresentedMediaTime(mediaTime) {
        if (!Number.isFinite(mediaTime)) return false;
        if (Number.isFinite(this.committedSeekMediaTime) &&
          Math.abs(mediaTime - this.committedSeekMediaTime) > decodedSeekFrameToleranceSeconds) {
          return false;
        }
        this.protectedMediaTime = mediaTime;
        this.committedSeekMediaTime = null;
        return true;
      }
      stopStatusPolling() {
        this.statusSeq++;
        this.statusTarget = null;
        if (this.statusTimer) clearTimeout(this.statusTimer);
        this.statusTimer = null;
        if (this.statusController) this.statusController.abort();
        this.statusController = null;
      }
    }
    const playback = new PlaybackSession();
    const seekCommitDelayMs = 250;
    const downloadFallbackInitialMs = 5000;
    const downloadFallbackPollingMs = 30000;
    const extendOptions = [
      { days: 1, label: '1 day' },
      { days: 7, label: '7 days' },
      { days: 30, label: '30 days' },
    ];
    function destroyVideoAndHls({
      resetLayout = true,
      preserveTransport = false,
    } = {}) {
      if (playback.events) { playback.events.abort(); playback.events = null; }
      if (playback.frameCallback !== null && $('video')?.cancelVideoFrameCallback) {
        $('video').cancelVideoFrameCallback(playback.frameCallback);
      }
      playback.frameCallback = null;
      if (playback.frameTimer) clearInterval(playback.frameTimer);
      playback.frameTimer = null;
      finishPlaybackStall(performance.now());
      if (!preserveTransport) {
        playback.playIntent = false;
        playback.autoplayBlocked = false;
      }
      playback.cancelSeekCommit();
      if (hasQoeData()) publishQoeSummary();
      playback.stallStartedAt = 0;
      if (playback.hls) { playback.hls.destroy(); playback.hls = null; }
      playback.stopStatusPolling();
      playback.streamDir = null;
      playback.generation = '';
      playback.attaching = false;
      playback.protectedMediaTime = null;
      playback.committedSeekMediaTime = null;
      playback.restoringPlayhead = false;
      playback.timeline.reset();
      const mediaDialog = $('mediaInfoDialog');
      if (mediaDialog?.open) mediaDialog.close();
      if (resetLayout) document.body.classList.remove('has-player');
      const video = $('video');
      if (resetLayout) {
        video.pause();
        video.removeAttribute('src');
        video.load();
      }
    }
    function setOptions(select, options) {
      select.replaceChildren(...options.map(({ value, label }) => new Option(label, String(value))));
    }
    function setFileList(files) {
      setOptions($('filelist'), files.map(f => ({
        value: f.index,
        label: `${f.name} (${formatBytes(f.size)})`,
      })));
    }
    function showMsg(id, msg, isErr=false, loader=false) {
      clearTimeout(msgTimeout);
      const el = $(id);
      el.textContent = '';
      if (loader) {
        const spinner = document.createElement('span');
        spinner.className = 'loader';
        el.appendChild(spinner);
      }
      if (msg) el.appendChild(document.createTextNode(msg));
      el.className = 'msg' + (isErr ? ' error' : '');
      const shouldAutoClear = msg && !isErr && !loader;
      if (shouldAutoClear) {
        msgTimeout = setTimeout(() => { el.textContent = ''; }, 2200);
      }
    }
    function showWarning(title = 'Preparing video', detail = 'The server is downloading and preparing the requested part.', status = 'Please stay on this page until playback begins.') {
      clearSubtitleWait();
      $('warn-title').textContent = title;
      $('warn-detail').textContent = detail;
      $('warn-status').textContent = status;
      $('warnMsg').hidden = false;
    }
    function setWarningExtra(msg) {
      const extra = document.getElementById('warn-extra');
      if (!extra) return;
      extra.textContent = msg || '';
      extra.hidden = !msg;
    }
    function clearSubtitleWait() {
      if (subtitleWaitTimer) {
        clearTimeout(subtitleWaitTimer);
        subtitleWaitTimer = null;
      }
      setWarningExtra('');
    }
    function removeWarning() {
      playback.stopStatusPolling();
      clearSubtitleWait();
      $('warnMsg').hidden = true;
    }

    function formatElapsed(startedAt) {
      const started = new Date(startedAt).getTime();
      if (!Number.isFinite(started)) return '';
      const seconds = Math.max(0, Math.floor((Date.now() - started) / 1000));
      if (seconds < 60) return `${seconds}s`;
      return `${Math.floor(seconds / 60)}m ${seconds % 60}s`;
    }

    function formatPlaybackTime(seconds) {
      if (!Number.isFinite(seconds) || seconds < 0) return '0:00';
      const whole = Math.floor(seconds);
      const hours = Math.floor(whole / 3600);
      const minutes = Math.floor((whole % 3600) / 60);
      const rest = whole % 60;
      return hours > 0
        ? `${hours}:${String(minutes).padStart(2, '0')}:${String(rest).padStart(2, '0')}`
        : `${minutes}:${String(rest).padStart(2, '0')}`;
    }

    function renderHlsStatus(status, fallbackTarget) {
      if (Number(status.duration) > 0 && playback.mediaInfo && !(Number(playback.mediaInfo.duration) > 0)) {
        playback.mediaInfo.duration = Number(status.duration);
        playback.mediaSeekable = true;
      }
      if (status.mode && status.mode !== playback.mode) {
        if (status.mode === 'transcode' && playback.usesPassthrough) {
          playback.usesPassthrough = false;
          playback.decision = 'The source could not be remuxed safely; the server switched to the compatibility fallback.';
        }
        playback.mode = status.mode;
        renderMediaInfo();
      }
      const reportedTarget = Number.isFinite(status.targetSeconds) ? status.targetSeconds : fallbackTarget;
      const staleReady = status.phase === 'ready' && Math.abs(reportedTarget - fallbackTarget) >= 1;
      const target = staleReady ? fallbackTarget : reportedTarget;
      const elapsed = formatElapsed(status.startedAt);
      const peerText = `${status.activePeers || 0} active / ${status.totalPeers || 0} known peers`;
      const suffix = elapsed ? ` · ${elapsed}` : '';
      const preparation = status.mode === 'direct'
        ? 'remuxing without video transcoding'
        : status.mode === 'hybrid'
          ? 'copying video and converting audio'
          : 'transcoding';
      const progress = [];
      if (status.peerBytes) progress.push(`${formatBytes(status.peerBytes)} from peers`);
      if (status.sourceRateBitsPerSecond) progress.push(`${formatBitrate(status.sourceRateBitsPerSecond)} source rate`);
      if (status.cacheBytes) progress.push(`${formatBytes(status.cacheBytes)} cached`);
      if (status.bytesRead) progress.push(`${formatBytes(status.bytesRead)} fed to media worker`);
      if (status.publishedBytes) progress.push(`${formatBytes(status.publishedBytes)} published`);
      if (status.rangePieces) progress.push(`${status.missingPieces || 0}/${status.rangePieces} pieces missing`);
      progress.push(peerText);
      const progressText = `${progress.join(' · ')}${suffix}`;
      if (staleReady) {
        showWarning(
          `Requesting ${formatPlaybackTime(target)}`,
          'The player is selecting the HLS segment for the new position.',
          peerText,
        );
        return;
      }
      if (status.phase === 'no_peers') {
        showWarning(
          `Waiting for peers at ${formatPlaybackTime(target)}`,
          status.message || 'There are no active peers for the required torrent pieces. Peer discovery is still running.',
          progressText,
        );
        return;
      }
      if (status.phase === 'stalled') {
        showWarning(
          `Stalled at ${formatPlaybackTime(target)}`,
          status.message || 'Connected peers and the transcoder have not produced data recently.',
          `${progressText} · still retrying`,
        );
        return;
      }
      if (status.phase === 'error') {
        showWarning(
          `Stream failed at ${formatPlaybackTime(target)}`,
          status.message || 'The server could not generate the requested segment.',
          `Preparation stopped · ${progressText}`,
        );
        return;
      }
      if (status.phase === 'ready') {
        showWarning(
          `Position ${formatPlaybackTime(target)} is ready`,
          'The requested segment is prepared. Connecting it to the player.',
          progressText,
        );
        return;
      }
      if (status.stage === 'waiting_source' || status.stage === 'source_blocked') {
        showWarning(
          `Waiting for source at ${formatPlaybackTime(target)}`,
          status.stage === 'source_blocked'
            ? 'The media worker is briefly waiting for required torrent pieces and will yield its slot automatically if they do not arrive.'
            : 'The media worker yielded its slot while the required torrent pieces are downloaded and verified.',
          progressText,
        );
        return;
      }
      if (status.stage === 'waiting_cpu' || status.stage === 'queued') {
        showWarning(
          `Queued for playback at ${formatPlaybackTime(target)}`,
          status.message || 'Waiting for foreground media-worker capacity.',
          progressText,
        );
        return;
      }
      if (status.stage === 'publishing') {
        showWarning(
          `Publishing ${formatPlaybackTime(target)}`,
          'A complete media fragment is being added to the HLS presentation.',
          progressText,
        );
        return;
      }
      if (status.phase === 'waiting') {
        showWarning(
          `Starting ${formatPlaybackTime(target)}`,
          'Media information is ready. Waiting for the first complete HLS fragment.',
          progressText,
        );
        return;
      }
      showWarning(
        `Preparing ${formatPlaybackTime(target)}`,
        `The source data is ready; the server is ${preparation} the requested fragment.`,
        progressText,
      );
    }

    function hlsStatusError(response, message) {
      const error = new Error(message || `Status request failed (${response.status})`);
      error.status = response.status;
      return error;
    }

    function retryableHlsStatusError(error) {
      const status = Number(error?.status || 0);
      return status === 0 || status === 408 || status === 429 || status >= 500;
    }

    async function fetchHlsStatus(streamDir, targetSeconds, signal) {
      const statusURL = `/api/hls/status/${encodeURIComponent(streamDir)}?target=${encodeURIComponent(targetSeconds)}`;
      const response = await apiFetch(statusURL, {
        cache: 'no-store',
        signal,
      }, 10000);
      if (!response.ok) {
        throw hlsStatusError(response, (await response.text()).trim());
      }
      return response.json();
    }

    function recoverMissingHlsStream(streamDir, targetSeconds, statusSeq, error) {
      if (statusSeq !== playback.statusSeq || playback.streamDir !== streamDir) return;
      playback.stopStatusPolling();
      if (playback.missingStreamRecoveryAttempted) {
        showPlaybackError(error.message || 'HLS stream not found');
        return;
      }
      playback.missingStreamRecoveryAttempted = true;
      playback.streamDir = null;
      playback.generation = '';
      const requestSeq = playback.requestSeq;
      queueMicrotask(() => {
        if (playback.requestSeq !== requestSeq || playback.streamDir !== null) return;
        startPlayback(targetSeconds, {
          keepPlayerVisible: true,
          attemptKind: 'stream_recovery',
          missingStreamRecovery: true,
        });
      });
    }

    function recoverChangedHlsPresentation(targetSeconds) {
      if (!playback.attaching || playback.hasPresentedFrame || playback.presentationRecoveryAttempted) {
        return false;
      }
      playback.presentationRecoveryAttempted = true;
      const requestSeq = playback.requestSeq;
      destroyVideoAndHls({ resetLayout: false, preserveTransport: true });
      queueMicrotask(() => {
        if (playback.requestSeq !== requestSeq || playback.hls !== null) return;
        startPlayback(targetSeconds, {
          keepPlayerVisible: true,
          attemptKind: 'presentation_recovery',
          presentationRecovery: true,
        });
      });
      return true;
    }

    function beginHlsStatusPolling(targetSeconds, replace = false) {
      if (!playback.streamDir) return;
      targetSeconds = Number.isFinite(targetSeconds) ? Math.max(0, targetSeconds) : 0;
      if (playback.statusTarget !== null && (!replace || Math.abs(playback.statusTarget - targetSeconds) < 1)) return;
      playback.stopStatusPolling();
      playback.statusTarget = targetSeconds;
      const seq = playback.statusSeq;
      const streamDir = playback.streamDir;
      showWarning(
        `Preparing ${formatPlaybackTime(targetSeconds)}`,
        'Checking the cache and requesting the required torrent pieces.',
        'Waiting for server status…',
      );
      let consecutiveFailures = 0;
      const poll = async () => {
        if (seq !== playback.statusSeq || playback.streamDir !== streamDir) return;
        const controller = new AbortController();
        playback.statusController = controller;
        try {
          const status = await fetchHlsStatus(streamDir, targetSeconds, controller.signal);
          if (seq !== playback.statusSeq) return;
          consecutiveFailures = 0;
          classifyPendingSeek(status);
          renderHlsStatus(status, targetSeconds);
        } catch (error) {
          if (error.name !== 'AbortError' && seq === playback.statusSeq) {
            if (error.status === 404) {
              recoverMissingHlsStream(streamDir, targetSeconds, seq, error);
              return;
            }
            if (!retryableHlsStatusError(error)) {
              playback.stopStatusPolling();
              showPlaybackError(error.message);
              return;
            }
            consecutiveFailures++;
            if (consecutiveFailures >= 3) {
              showWarning(
                'Cannot reach the stream worker',
                error.message || 'The server stopped reporting preparation progress.',
                'Playback may have failed. Retrying status…',
              );
            } else {
              $('warn-status').textContent = 'Stream status is temporarily unavailable; retrying…';
            }
          }
        } finally {
          if (playback.statusController === controller) playback.statusController = null;
          if (seq === playback.statusSeq && playback.streamDir === streamDir) {
            playback.statusTimer = setTimeout(poll, 1000);
          }
        }
      };
      poll();
    }

    function classifyPendingSeek(status) {
      const attempt = playback.presentationAttempt;
      if (attempt?.kind !== 'seek' || attempt.seekClass !== 'pending') return;
      attempt.seekClass = status.phase === 'ready' ? 'cached' : 'cold';
    }

    async function waitForHlsReady(streamDir, targetSeconds, signal) {
      for (;;) {
        let status;
        try {
          status = await fetchHlsStatus(streamDir, targetSeconds, signal);
        } catch (error) {
          if (error.name === 'AbortError' || !retryableHlsStatusError(error)) throw error;
          await waitForRetry(signal, 500);
          continue;
        }
        classifyPendingSeek(status);
        renderHlsStatus(status, targetSeconds);
        if (status.phase === 'ready') return status;
        if (status.phase === 'error') {
          throw new Error(status.message || 'The server could not prepare the requested position');
        }
        await new Promise(resolve => setTimeout(resolve, 500));
        if (signal.aborted) throw new DOMException('Aborted', 'AbortError');
      }
    }

    function waitForRetry(signal, delayMs) {
      return new Promise((resolve, reject) => {
        if (signal.aborted) {
          reject(new DOMException('Aborted', 'AbortError'));
          return;
        }
        const timer = setTimeout(() => {
          signal.removeEventListener('abort', abort);
          resolve();
        }, delayMs);
        const abort = () => {
          clearTimeout(timer);
          reject(new DOMException('Aborted', 'AbortError'));
        };
        signal.addEventListener('abort', abort, { once: true });
      });
    }

    async function prepareHlsDescriptor(url, signal) {
      for (;;) {
        const response = await apiFetch(url, {
          headers: { Accept: 'application/json' },
          signal,
        }, 60000);
        if (response.ok) return response.json();
        const message = (await response.text()).trim() || 'Stream error';
        if (response.status !== 429 && response.status !== 503) {
          throw new Error(message);
        }
        showWarning(
          'Queued for playback',
          message,
          'The requested position is retained. Retrying automatically…',
        );
        const retryAfter = Number(response.headers.get('Retry-After'));
        const delayMs = Number.isFinite(retryAfter) && retryAfter > 0
          ? Math.min(retryAfter * 1000, 5000)
          : 500;
        await waitForRetry(signal, delayMs);
      }
    }

    function showPlaybackNotice() {
      showMsg('playerMsg', playback.notice);
      if (playback.notice) {
        clearTimeout(msgTimeout);
      }
    }

    function formatBytes(bytes) {
      if (!Number.isFinite(bytes) || bytes <= 0) return '0 B';
      const units = ['B', 'KB', 'MB', 'GB', 'TB'];
      let value = bytes;
      let idx = 0;
      while (value >= 1024 && idx < units.length - 1) {
        value /= 1024;
        idx++;
      }
      const digits = value >= 10 || idx === 0 ? 0 : 1;
      return `${value.toFixed(digits)} ${units[idx]}`;
    }

    function formatBitrate(bits) {
      if (!Number.isFinite(bits) || bits <= 0) return '';
      return `${(bits / 1000000).toFixed(bits >= 10000000 ? 1 : 2)} Mbps`;
    }

    function formatResolution(info) {
      const width = Number(info?.width);
      const height = Number(info?.height);
      if (!width || !height) return '';
      const label = width >= 7000 ? '8K' : width >= 3800 ? '4K' : `${height}p`;
      return `${label} (${width}×${height})`;
    }

    function formatCodecLevel(info) {
      const level = Number(info?.videoLevel);
      if (!level) return '';
      if (info.videoCodec === 'hevc') {
        return `Level ${Math.floor(level / 30)}.${Math.floor((level % 30) / 3)}`;
      }
      if (info.videoCodec === 'av1') {
        return `Level ${2 + Math.floor(level / 4)}.${level % 4}`;
      }
      return `Level ${(level / 10).toFixed(1)}`;
    }

    function formatCodec(codec) {
      const names = { h264: 'H.264', hevc: 'HEVC', av1: 'AV1', aac: 'AAC', ac3: 'AC-3', eac3: 'E-AC-3', dts: 'DTS', truehd: 'TrueHD', opus: 'Opus' };
      return names[codec] || String(codec || '').toUpperCase();
    }

    function selectedAudioTrack() {
      const tracks = playback.mediaInfo?.audioTracks || [];
      const selected = Number.parseInt($('audioSelect').value || '0', 10);
      return tracks.find(track => track.index === selected) || tracks[0] || null;
    }

    function setMediaInfoRow(rowID, valueID, value) {
      const row = $(rowID);
      const target = $(valueID);
      if (!row || !target) return;
      row.hidden = !value;
      target.textContent = value || '';
    }

    function renderMediaInfo() {
      const info = playback.mediaInfo;
      const button = $('mediaInfoBtn');
      if (!button) return;
      button.hidden = !info;
      if (!info) return;

      const video = [formatResolution(info), formatCodec(info.videoCodec), info.videoProfile, formatCodecLevel(info), info.frameRate ? `${info.frameRate.toFixed(3).replace(/\.0+$/, '')} fps` : ''].filter(Boolean).join(' · ');
      const color = [info.hdrFormat || (info.hdr ? 'HDR' : 'SDR'), info.bitDepth ? `${info.bitDepth}-bit` : '', info.pixelFormat, info.colorPrimaries, info.colorTransfer].filter(Boolean).join(' · ');
      const audio = selectedAudioTrack();
      const channels = audio?.channels === 1 ? 'mono' : audio?.channels === 2 ? 'stereo' : audio?.channels === 6 ? '5.1' : audio?.channels === 8 ? '7.1' : audio?.channels ? `${audio.channels} channels` : '';
      const audioText = audio ? [formatCodec(audio.codec), audio.profile, channels, audio.sampleRate ? `${audio.sampleRate / 1000} kHz` : '', audio.language].filter(Boolean).join(' · ') : 'No audio';
      const modes = {
        direct: audio ? 'Original video and audio' : 'Original video · no audio',
        hybrid: 'Original video · audio converted to AAC',
        transcode: `H.264 SDR · ${formatResolution(info) || 'source resolution'}`,
      };

      setMediaInfoRow('mediaVideoRow', 'mediaVideo', video);
      setMediaInfoRow('mediaColorRow', 'mediaColor', color);
      setMediaInfoRow('mediaBitrateRow', 'mediaBitrate', formatBitrate(Number(info.bitrate)));
      setMediaInfoRow('mediaAudioRow', 'mediaAudio', audioText);
      $('mediaPlaybackMode').textContent = modes[playback.mode] || 'Preparing';
      setMediaInfoRow('mediaDecisionRow', 'mediaDecision', playback.decision);
    }

    function setMediaInfo(info = null) {
      playback.mediaInfo = info;
      playback.mediaSeekable = info?.seekable === true;
      playback.timeline.reset();
      playback.mode = '';
      playback.decision = '';
      playback.usesPassthrough = false;
      playback.forceTranscode = false;
      playback.nativeRetries = 0;
      playback.notice = '';
      renderMediaInfo();
    }

    function useCompatibilityFallback(reason) {
      if (!playback.usesPassthrough) return;
      playback.forceTranscode = true;
      playback.decision = reason || 'The original stream failed to decode; using the compatibility fallback.';
      renderMediaInfo();
    }

    async function sourcePlaybackCapability(info) {
      const codec = info?.videoCodec;
      if (!['h264', 'hevc', 'av1'].includes(codec)) {
        return { supported: false, reason: `${formatCodec(codec)} requires the compatibility fallback.` };
      }
      if (info.dolbyVision) {
        return { supported: false, reason: 'Dolby Vision passthrough is not yet safely detectable in browsers.' };
      }
      if (info.interlaced || info.rotated) {
        return { supported: false, reason: 'The source requires a video transform before playback.' };
      }
      const subtitleIndex = Number.parseInt($('subtitleSelect').value || '-1', 10);
      const subtitle = (info.subtitles || []).find(track => track.index === subtitleIndex);
      if (subtitle && ['hdmv_pgs_subtitle', 'dvd_subtitle', 'dvb_subtitle', 'xsub'].includes(subtitle.codec)) {
        return { supported: false, reason: 'The selected bitmap subtitles must be rendered into the video.' };
      }
      const profile = String(info.videoProfile || '').toLowerCase();
      const pixelFormat = String(info.pixelFormat || '');
      const directPixelFormat = !pixelFormat || pixelFormat === 'yuv420p' || (codec !== 'h264' && pixelFormat === 'yuv420p10le');
      const directProfile = codec === 'h264'
        ? !profile || ['baseline', 'constrained baseline', 'main', 'high'].includes(profile)
        : codec === 'hevc'
          ? !profile || ['main', 'main 10'].includes(profile)
          : !profile || profile === 'main';
      const level = Number(info.videoLevel) || 0;
      const directLevel = codec === 'h264' ? level <= 62 : codec === 'hevc' ? level <= 186 : level <= 18;
      if (!directPixelFormat || !directProfile || !directLevel || (codec === 'h264' && info.hdr)) {
        return { supported: false, reason: 'This profile or pixel format requires the compatibility fallback.' };
      }
      const video = $('video');
      const hlsJs = Boolean(window.Hls && Hls.isSupported());
      const nativeHls = Boolean(video.canPlayType('application/vnd.apple.mpegurl'));
      if (!hlsJs && !nativeHls) {
        return { supported: false, reason: 'This browser has no HLS playback path.' };
      }
      if (!hlsJs) {
        return { supported: false, reason: 'Native HLS decoder support cannot be verified precisely, so this browser uses the compatibility fallback.' };
      }
      if (info.hdr && window.matchMedia && !window.matchMedia('(dynamic-range: high)').matches) {
        return { supported: false, reason: 'The current display path does not report HDR output support.' };
      }

      const fallbackCodecs = { h264: 'avc1.640028', hevc: 'hvc1.1.6.L93.B0', av1: 'av01.0.08M.08' };
      const contentType = `video/mp4; codecs="${info.videoCodecString || fallbackCodecs[codec]}"`;
      const mediaSource = hlsJs && (Hls.getMediaSource ? Hls.getMediaSource() : window.MediaSource);
      const typeSupported = mediaSource?.isTypeSupported
        ? mediaSource.isTypeSupported(contentType)
        : video.canPlayType(contentType) !== '';
      if (!typeSupported) {
        return { supported: false, reason: `${formatCodec(codec)} is not supported by this browser's media pipeline.` };
      }

      if (navigator.mediaCapabilities?.decodingInfo) {
        try {
          const videoConfiguration = {
            contentType,
            width: Number(info.width) || 1920,
            height: Number(info.height) || 1080,
            bitrate: Number(info.bitrate) || 5500000,
            framerate: Number(info.frameRate) || 30,
          };
          if (info.hdr) {
            videoConfiguration.transferFunction = info.colorTransfer === 'arib-std-b67' ? 'hlg' : 'pq';
            if (info.colorPrimaries === 'bt2020') videoConfiguration.colorGamut = 'rec2020';
            if (info.hdrFormat === 'HDR10') videoConfiguration.hdrMetadataType = 'smpteSt2086';
            if (info.hdrFormat === 'HDR10+') videoConfiguration.hdrMetadataType = 'smpteSt2094-40';
          }
          const result = await navigator.mediaCapabilities.decodingInfo({
            type: hlsJs ? 'media-source' : 'file',
            video: videoConfiguration,
          });
          if (!result.supported || !result.smooth || !result.powerEfficient) {
            const reason = !result.supported ? 'unsupported' : !result.smooth ? 'not expected to play smoothly' : 'not hardware-efficient';
            return { supported: false, reason: `The original ${formatCodec(codec)} stream is ${reason} on this device.` };
          }
        } catch (_) {
          // The MIME check above remains the conservative fallback on partial implementations.
        }
      }
      return { supported: true, reason: 'Original stream supported by this browser and device.' };
    }

    function formatExpiry(value) {
      const expires = new Date(value);
      if (!Number.isFinite(expires.getTime())) return 'no expiry';
      const diffMs = expires.getTime() - Date.now();
      const absMs = Math.abs(diffMs);
      const units = [
        { label: 'd', ms: 24 * 60 * 60 * 1000 },
        { label: 'h', ms: 60 * 60 * 1000 },
        { label: 'm', ms: 60 * 1000 },
      ];
      const unit = units.find(u => absMs >= u.ms) || units[units.length - 1];
      const valueRounded = Math.max(1, Math.round(absMs / unit.ms));
      return diffMs < 0 ? `expired ${valueRounded}${unit.label} ago` : `${valueRounded}${unit.label} left`;
    }

    function formatDownloadSize(download) {
      return `${formatBytes(download.size)} torrent · shared cache`;
    }

    function closeExtendMenus(except = null) {
      let preservedID = null;
      document.querySelectorAll('.download-menu.open').forEach(menu => {
        const toggle = menu.querySelector('button[data-action="toggle-extend"]');
        if (menu === except) {
          preservedID = toggle ? toggle.dataset.id : openExtendDownloadID;
          return;
        }
        menu.classList.remove('open');
        const popup = menu.querySelector('.download-extend-menu');
        if (popup) popup.hidden = true;
        if (toggle) toggle.setAttribute('aria-expanded', 'false');
      });
      openExtendDownloadID = preservedID;
    }

    function toggleExtendMenu(toggle) {
      const menu = toggle.closest('.download-menu');
      if (!menu) return;
      const popup = menu.querySelector('.download-extend-menu');
      if (!popup) return;
      const willOpen = popup.hidden;
      closeExtendMenus(menu);
      popup.hidden = !willOpen;
      menu.classList.toggle('open', willOpen);
      toggle.setAttribute('aria-expanded', String(willOpen));
      openExtendDownloadID = willOpen ? toggle.dataset.id : null;
    }

    function openExtendMenu(menu) {
      const popup = menu.querySelector('.download-extend-menu');
      const toggle = menu.querySelector('button[data-action="toggle-extend"]');
      if (!popup || !toggle) return;
      popup.hidden = false;
      menu.classList.add('open');
      toggle.setAttribute('aria-expanded', 'true');
      openExtendDownloadID = toggle.dataset.id;
    }

    function createExtendMenu(downloadID) {
      const extendMenu = document.createElement('div');
      extendMenu.className = 'download-menu download-expiry-menu';
      const extendBtn = document.createElement('button');
      extendBtn.type = 'button';
      extendBtn.className = 'download-expiry-extend';
      extendBtn.dataset.action = 'toggle-extend';
      extendBtn.dataset.id = downloadID;
      extendBtn.title = 'Extend download';
      extendBtn.setAttribute('aria-label', 'Extend download');
      extendBtn.setAttribute('aria-haspopup', 'menu');
      extendBtn.setAttribute('aria-expanded', 'false');
      extendBtn.textContent = '+';

      const popup = document.createElement('div');
      popup.className = 'download-extend-menu';
      popup.setAttribute('role', 'menu');
      popup.hidden = true;
      extendOptions.forEach(({ days, label }) => {
        const item = document.createElement('button');
        item.type = 'button';
        item.className = 'download-menu-item';
        item.dataset.action = 'extend';
        item.dataset.id = downloadID;
        item.dataset.days = String(days);
        item.setAttribute('role', 'menuitem');
        item.textContent = label;
        popup.appendChild(item);
      });
      extendMenu.append(extendBtn, popup);
      return extendMenu;
    }

    function renderDownloads(downloads) {
      const list = $('downloadsList');
      const restoreExtendID = openExtendDownloadID;
      let restoredExtendMenu = false;
      list.textContent = '';
      if (!downloads.length) {
        openExtendDownloadID = null;
        const empty = document.createElement('div');
        empty.className = 'downloads-empty';
        empty.textContent = 'No downloads yet';
        list.appendChild(empty);
        return;
      }

      downloads.forEach(download => {
        const row = document.createElement('div');
        row.className = 'download-row';
        row.dataset.id = download.id;

        const main = document.createElement('button');
        main.type = 'button';
        main.className = 'download-main download-open';
        main.dataset.action = 'open';
        main.dataset.id = download.id;
        const titleRow = document.createElement('div');
        titleRow.className = 'download-title-row';
        const title = document.createElement('div');
        title.className = 'download-title';
        title.textContent = download.title || `Torrent ${download.id.slice(0, 8)}`;
        titleRow.appendChild(title);
        const subtitle = document.createElement('div');
        subtitle.className = 'download-subtitle';
        subtitle.textContent = download.id;
        main.append(titleRow, subtitle);

        const meta = document.createElement('div');
        meta.className = 'download-meta';
        const metaText = document.createElement('span');
        metaText.className = 'download-meta-text';
        metaText.textContent = `${formatDownloadSize(download)} · ${formatExpiry(download.expiresAt)}`;
        const extendMenu = createExtendMenu(download.id);
        if (download.id === restoreExtendID) {
          openExtendMenu(extendMenu);
          restoredExtendMenu = true;
        }
        meta.append(metaText, extendMenu);

        const actions = document.createElement('div');
        actions.className = 'download-actions';

        const deleteBtn = document.createElement('button');
        deleteBtn.type = 'button';
        deleteBtn.className = 'download-action input-style delete icon-only';
        deleteBtn.dataset.action = 'delete';
        deleteBtn.dataset.id = download.id;
        deleteBtn.title = 'Delete';
        deleteBtn.setAttribute('aria-label', 'Delete');

        actions.append(deleteBtn);

        row.append(main, meta, actions);
        list.appendChild(row);
      });
      if (!restoredExtendMenu) openExtendDownloadID = null;
    }

    async function loadDownloads({ quiet = false, suppressError = false } = {}) {
      if (downloadsLoading) {
        downloadsRefreshQueued = true;
        return;
      }
      downloadsLoading = true;
      if (!quiet) showMsg('downloadsMsg', 'Loading downloads...', false, true);
      try {
        const res = await apiFetch('/api/downloads');
        if (!res.ok) throw new Error('Could not load downloads');
        downloadCatalog = await res.json();
        renderDownloads(downloadCatalog);
        if (!quiet) showMsg('downloadsMsg', '');
      } catch (e) {
        if (!suppressError) {
          showMsg('downloadsMsg', e.message || 'Could not load downloads', true);
        }
      } finally {
        downloadsLoading = false;
        if (downloadsRefreshQueued) {
          downloadsRefreshQueued = false;
          loadDownloads({ quiet: true, suppressError: true });
        }
      }
    }

    function findDownload(id) {
      return downloadCatalog.find(download => download.id === id);
    }

    function openDownload(download) {
      if (!download || !download.magnet || !download.files || download.files.length === 0) {
        showMsg('downloadsMsg', 'Download metadata is incomplete', true);
        return;
      }
      playback.cancelRequest();
      destroyVideoAndHls();
      setMediaInfo();
      $('magnet').value = download.magnet;
      setFileList(download.files);
      $('step-files').style.display = '';
      $('step-tracks').style.display = 'none';
      $('player-block').style.display = 'none';
      showMsg('magnetMsg', '');
      showMsg('fileMsg', '');
      showMsg('trackMsg', '');
      showMsg('downloadsMsg', '');
      removeWarning();
    }

    async function extendDownload(id, days) {
      showMsg('downloadsMsg', 'Extending download...', false, true);
      try {
        const res = await apiFetch(`/api/downloads/${encodeURIComponent(id)}/extend`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ days }),
        });
        if (!res.ok) throw new Error('Could not extend download');
        await loadDownloads({ quiet: true });
        showMsg('downloadsMsg', `Download extended by ${days}d`);
      } catch (e) {
        showMsg('downloadsMsg', e.message || 'Could not extend download', true);
      }
    }

    async function deleteDownload(id) {
      const download = findDownload(id);
      if (!download || !window.confirm(`Delete ${download.title || id}?`)) return;
      showMsg('downloadsMsg', 'Deleting download...', false, true);
      try {
        const res = await apiFetch(`/api/downloads/${encodeURIComponent(id)}`, { method: 'DELETE' });
        if (!res.ok) throw new Error('Could not delete download');
        if ($('magnet').value.trim() === download.magnet) {
          playback.cancelRequest();
          destroyVideoAndHls();
          setMediaInfo();
          $('magnet').value = '';
          $('filelist').textContent = '';
          $('step-files').style.display = 'none';
          $('step-tracks').style.display = 'none';
          $('player-block').style.display = 'none';
          removeWarning();
        }
        await loadDownloads({ quiet: true });
        showMsg('downloadsMsg', 'Download deleted');
      } catch (e) {
        showMsg('downloadsMsg', e.message || 'Could not delete download', true);
      }
    }

    function stopDownloadFallbackPolling() {
      downloadFallbackPolling = false;
      if (downloadPollingTimer) {
        clearTimeout(downloadPollingTimer);
        downloadPollingTimer = null;
      }
    }

    function scheduleDownloadFallbackPolling(delay = downloadFallbackPollingMs) {
      downloadFallbackPolling = true;
      if (downloadPollingTimer) return;
      downloadPollingTimer = setTimeout(() => {
        downloadPollingTimer = null;
        if (!downloadFallbackPolling) return;
        if (document.visibilityState !== 'hidden') {
          loadDownloads({ quiet: true, suppressError: true });
        }
        scheduleDownloadFallbackPolling(downloadFallbackPollingMs);
      }, delay);
    }

    function startDownloadEvents() {
      if (!window.EventSource) {
        scheduleDownloadFallbackPolling(0);
        return;
      }

      const events = new EventSource('/api/downloads/events');
      downloadEvents = events;
      events.addEventListener('open', () => {
        if (downloadEvents === events) stopDownloadFallbackPolling();
      });
      events.addEventListener('changed', () => {
        loadDownloads({ quiet: true, suppressError: true });
      });
      events.addEventListener('error', () => {
        if (downloadEvents === events) scheduleDownloadFallbackPolling(downloadFallbackInitialMs);
      });
    }

    $('downloadsList').addEventListener('click', e => {
      const extendToggle = e.target.closest('button[data-action="toggle-extend"][data-id]');
      if (extendToggle) {
        toggleExtendMenu(extendToggle);
        return;
      }
      const btn = e.target.closest('button[data-action][data-id]');
      if (btn) {
        if (btn.dataset.action === 'open') openDownload(findDownload(btn.dataset.id));
        if (btn.dataset.action === 'extend') {
          closeExtendMenus();
          extendDownload(btn.dataset.id, parseInt(btn.dataset.days || '7', 10));
        }
        if (btn.dataset.action === 'delete') deleteDownload(btn.dataset.id);
        return;
      }
    });
    document.addEventListener('click', e => {
      if (!e.target.closest('.download-menu')) closeExtendMenus();
    });
    document.addEventListener('keydown', e => {
      if (e.key === 'Escape') closeExtendMenus();
    });
    document.addEventListener('visibilitychange', () => {
      if (document.visibilityState === 'visible') {
        loadDownloads({ quiet: true, suppressError: true });
      }
    });
    window.addEventListener('beforeunload', () => {
      if (downloadEvents) downloadEvents.close();
    });
    loadDownloads({ quiet: true, suppressError: true });
    startDownloadEvents();

    $('form').onsubmit = async e => {
      e.preventDefault();
      const magnet = $('magnet').value.trim();
      if (!magnet) {
        showMsg('magnetMsg', 'Magnet link required', true);
        return;
      }
      const request = playback.beginRequest();
      const requestId = request.id;
      destroyVideoAndHls();
      setMediaInfo();
      showMsg('magnetMsg', 'Loading file list…', false, true);
      $('filelist').textContent = '';
      $('step-files').style.display = 'none';
      $('step-tracks').style.display = 'none';
      $('player-block').style.display = 'none';
      removeWarning();
      try {
        const res = await apiFetch('/api/torrent/files?magnet=' + encodeURIComponent(magnet), { signal: request.signal }, 60000);
        if (!res.ok) throw new Error((await res.text()).trim() || 'Server error');
        const files = await res.json();
        if (playback.isStale(requestId)) return;
        if (!files.length) throw new Error('No playable files found in torrent');
        setFileList(files);
        $('step-files').style.display = '';
        showMsg('magnetMsg', '');
      } catch (e) {
        if (e.name === 'AbortError') return;
        if (playback.isStale(requestId)) return;
        showMsg('magnetMsg', e.message || 'Error loading files', true);
        return;
      } finally {
        playback.finishRequest(request);
      }
    };

    function formatTrackLabel(track, type) {
      let label = type === 'audio' ? `Track ${track.index + 1}` : `Subtitle ${track.index + 1}`;
      if (track.language) label += ` [${track.language}]`;
      if (track.title) label += ` - ${track.title}`;
      if (track.codec) label += ` (${track.codec})`;
      return label;
    }

    $('selectTracks').onclick = async () => {
      const magnet = $('magnet').value.trim();
      const idx = $('filelist').value;
      if (!magnet || idx === undefined || idx === '') {
        showMsg('fileMsg', 'Select a file first', true);
        return;
      }
      const request = playback.beginRequest();
      const requestId = request.id;
      showMsg('fileMsg', 'Loading track info…', false, true);
      $('step-tracks').style.display = 'none';
      try {
        const res = await apiFetch(`/api/media/info?magnet=${encodeURIComponent(magnet)}&file=${idx}`, { signal: request.signal }, 60000);
        if (!res.ok) throw new Error((await res.text()).trim() || 'Could not load media info');
        const info = await res.json();
        if (playback.isStale(requestId)) return;
        setMediaInfo(info);
        const notices = Array.isArray(info.warnings) ? [...info.warnings] : [];
        if (!playback.mediaSeekable) {
          notices.unshift('Duration is unavailable for this format. Playback is progressive; seeking is limited to the discovered part.');
        }
        playback.notice = notices.join(' ');

        const audioCount = info.audioTracks ? info.audioTracks.length : 0;
        const subCount = info.subtitles ? info.subtitles.length : 0;
        const audioSelect = $('audioSelect');
        const subSelect = $('subtitleSelect');
        const audioRow = audioSelect.closest('.track-row');
        const subRow = subSelect.closest('.track-row');

        // If nothing to choose, play immediately
        if (audioCount <= 1 && subCount === 0) {
          showMsg('fileMsg', '');
          setOptions(audioSelect, [{ value: 0, label: 'Default' }]);
          setOptions(subSelect, [{ value: -1, label: 'None' }]);
          if (audioRow) audioRow.style.display = 'none';
          if (subRow) subRow.style.display = 'none';
          $('play').click();
          return;
        }

        // Audio tracks
        if (audioCount > 1) {
          setOptions(audioSelect, info.audioTracks.map(t => ({
            value: t.index,
            label: formatTrackLabel(t, 'audio'),
          })));
          if (audioRow) audioRow.style.display = '';
        } else {
          setOptions(audioSelect, [{ value: 0, label: 'Default' }]);
          if (audioRow) audioRow.style.display = 'none';
        }

        // Subtitles
        setOptions(subSelect, [{ value: -1, label: 'None' }]);
        if (subCount > 0) {
          setOptions(subSelect, [
            { value: -1, label: 'None' },
            ...info.subtitles.map(t => ({
              value: t.index,
              label: formatTrackLabel(t, 'subtitle'),
            })),
          ]);
          if (subRow) subRow.style.display = '';
          $('subtitleDelayRow').style.display = '';
        } else {
          if (subRow) subRow.style.display = 'none';
          $('subtitleDelayRow').style.display = 'none';
        }

        $('step-tracks').style.display = '';
        showMsg('fileMsg', '');
      } catch (e) {
        if (e.name === 'AbortError') return;
        if (playback.isStale(requestId)) return;
        showMsg('fileMsg', e.message || 'Error loading tracks', true);
      } finally {
        playback.finishRequest(request);
      }
    };

    async function startPlayback(resumeTime = 0, {
      keepPlayerVisible = false,
      attemptKind = '',
      missingStreamRecovery = false,
      presentationRecovery = false,
    } = {}) {
      playback.cancelSeekCommit();
      const magnet = $('magnet').value.trim();
      const idx = $('filelist').value;
      const audio = $('audioSelect').value || '0';
      const subtitle = $('subtitleSelect').value || '-1';
      const subtitleSelected = parseInt(subtitle, 10) >= 0;
      if (!magnet || idx === undefined || idx === '') {
        showMsg('trackMsg', 'Select a file first', true);
        return;
      }
      const start = playback.mediaSeekable && Number.isFinite(resumeTime) && resumeTime > 0 ? resumeTime : 0;
      if (!missingStreamRecovery) playback.missingStreamRecoveryAttempted = false;
      if (!presentationRecovery) playback.presentationRecoveryAttempted = false;
      const request = playback.beginRequest(start);
      playback.stopStatusPolling();
      const requestId = request.id;
      const wasPlaying = $('player-block').style.display !== 'none' || document.body.classList.contains('has-player');
      const retainedHls = keepPlayerVisible ? playback.hls : null;
      if (retainedHls?.stopLoad) {
        retainedHls.stopLoad();
      } else {
        destroyVideoAndHls({ resetLayout: !wasPlaying, preserveTransport: true });
      }
      const kind = attemptKind || (wasPlaying ? 'seek' : 'startup');
      beginPresentationAttempt(kind, start, kind === 'seek' ? 'pending' : '');
      $('player-block').style.display = keepPlayerVisible ? '' : 'none';
      removeWarning();
      showWarning();
      showMsg('trackMsg', '');
      showMsg('playerMsg', keepPlayerVisible ? 'Restoring stream...' : '', false, keepPlayerVisible);
      playback.lastActivityAt = Date.now();
      if (subtitleSelected) {
        clearSubtitleWait();
        subtitleWaitTimer = setTimeout(() => {
          setWarningExtra('Waiting for subtitles...');
        }, 8000);
      }
      try {
        const capability = await sourcePlaybackCapability(playback.mediaInfo);
        if (playback.isStale(requestId)) return;
        const forceTranscode = playback.forceTranscode || !capability.supported;
        playback.usesPassthrough = !forceTranscode;
        playback.mode = '';
        playback.decision = playback.forceTranscode
          ? 'The original stream failed to decode; using the compatibility fallback.'
          : capability.reason;
        renderMediaInfo();
        const prepared = await prepareHlsDescriptor(
          `/api/hls/prepare?magnet=${encodeURIComponent(magnet)}&file=${idx}&audio=${audio}&subtitle=${subtitle}&start=${encodeURIComponent(start)}&transcode=${forceTranscode ? 1 : 0}`,
          request.signal,
        );
        if (playback.isStale(requestId)) return;
        if (!prepared.playlist || !prepared.stream) throw new Error('Server returned an invalid stream descriptor');
        $('player-block').style.display = '';
        document.body.classList.add('has-player');
        const readyStatus = await waitForHlsReady(prepared.stream, start, request.signal);
        if (playback.isStale(requestId)) return;
        const duration = Number(playback.mediaInfo?.duration);
        const presentationOrigin = Number(readyStatus.presentationOriginSeconds);
        if (playback.mediaSeekable && Number.isFinite(duration) && duration > 0 &&
          (!Number.isFinite(presentationOrigin) || presentationOrigin < 0)) {
          throw new Error('Server returned an invalid presentation origin');
        }
        const playlistURL = new URL(prepared.playlist, window.location.href);
        const reusesPresentation = retainedHls && playback.hls === retainedHls &&
          prepared.stream === playback.streamDir &&
          readyStatus.generation && readyStatus.generation === playback.generation;
        if (retainedHls && playback.hls === retainedHls && !reusesPresentation) {
          destroyVideoAndHls({ resetLayout: false, preserveTransport: true });
        }
        playback.streamDir = prepared.stream;
        playback.generation = readyStatus.generation || '';
        showPlaybackNotice();
        beginHlsStatusPolling(start);
        if (reusesPresentation) {
          retainedHls.startLoad?.(playback.timeline.hlsTime(start));
          return;
        }
        if (playback.generation) playlistURL.searchParams.set('v', playback.generation);
        playlistURL.searchParams.set('t', Date.now());
        const m3u8 = playlistURL.pathname + playlistURL.search;
        const segmentSeconds = Number(prepared.segmentDurationSeconds) || 2;
        const windowSegments = Number(prepared.windowSegments) || 15;
        playHls(m3u8, subtitleSelected, {
          segmentSeconds,
          windowSegments,
          bitrate: Number(playback.mediaInfo?.bitrate),
          duration,
          sourceStart: start,
          presentationOrigin,
        });
      } catch (e) {
        if (e.name === 'AbortError') return;
        if (playback.isStale(requestId)) return;
        finishPresentationAttempt('error');
        publishQoeSummary();
        removeWarning();
        const msg = e.message || 'Could not start stream';
        if (keepPlayerVisible) {
          showMsg('playerMsg', msg, true);
        } else {
          showMsg('trackMsg', msg, true);
        }
        return;
      } finally {
        playback.finishRequest(request);
      }
    }

    $('play').onclick = () => {
      playback.forceTranscode = false;
      playback.nativeRetries = 0;
      playback.playIntent = true;
      syncPlaybackControls();
      startPlayback(0);
    };
    ['audioSelect', 'subtitleSelect'].forEach(id => {
      const el = $(id);
      if (!el) return;
      el.addEventListener('change', () => {
        playback.forceTranscode = false;
        playback.nativeRetries = 0;
        renderMediaInfo();
        if ($('player-block').style.display !== 'none') {
          const t = playback.timeline.sourceTime($('video')?.currentTime);
          startPlayback(t);
        }
      });
    });

    function applySubtitleDelay(video, delaySec) {
      if (!video || !video.textTracks) return;
      const track = Array.from(video.textTracks).find(t => t.kind === 'subtitles' || t.kind === 'captions');
      if (!track || !track.cues) return;
      for (let i = 0; i < track.cues.length; i++) {
        const cue = track.cues[i];
        if (cue.__appliedDelay === delaySec) continue;
        if (cue.__origStart === undefined) {
          cue.__origStart = cue.startTime;
          cue.__origEnd = cue.endTime;
        }
        cue.__appliedDelay = delaySec;
        cue.startTime = Math.max(0, cue.__origStart + delaySec);
        cue.endTime = Math.max(cue.startTime, cue.__origEnd + delaySec);
      }
    }

    function showSubtitleTextTracks(video) {
      if (!video || !video.textTracks) return;
      let enabled = false;
      Array.from(video.textTracks).forEach(track => {
        if (track.kind !== 'subtitles' && track.kind !== 'captions') return;
        track.mode = enabled ? 'disabled' : 'showing';
        enabled = true;
      });
    }

    function watchSubtitleUpdates(video, onUpdate, signal) {
      if (!video || !video.textTracks) return;
      const tracks = video.textTracks;
      const attachTrack = track => {
        if (!track || !track.addEventListener) return;
        track.addEventListener('cuechange', onUpdate, { signal });
      };
      Array.from(tracks).forEach(attachTrack);
      if (tracks.addEventListener) {
        tracks.addEventListener('addtrack', e => {
          attachTrack(e.track);
          showSubtitleTextTracks(video);
          setTimeout(onUpdate, 0);
        }, { signal });
      }
    }

    function retryStartupWithCompatibilityFallback(reason) {
      const video = $('video');
      if (!video || $('player-block').style.display === 'none') return false;
      if (playback.hasPresentedFrame || playback.requestController ||
        playback.forceTranscode || !playback.usesPassthrough) return false;
      useCompatibilityFallback(reason);
      const resumeTime = playback.timeline.sourceTime(video?.currentTime);
      startPlayback(resumeTime, { keepPlayerVisible: true, attemptKind: 'startup_fallback' });
      return true;
    }

    function showPlaybackError(details) {
      removeWarning();
      showMsg('playerMsg', 'Playback error: ' + (details || 'Fatal error'), true);
    }

    function beginPresentationAttempt(kind, target, seekClass = '') {
      const now = performance.now();
      updateQoePlayingTime(now);
      finishPresentationAttempt('cancelled', now);
      playback.presentationAttempt = {
        id: ++playback.presentationAttemptSeq,
        kind,
        target,
        startedAt: now,
        ...(seekClass ? { seekClass } : {}),
      };
    }

    function finishPresentationAttempt(result, now = performance.now()) {
      const attempt = playback.presentationAttempt;
      if (!attempt) return false;
      attempt.result = result;
      attempt.latencyMs = now - attempt.startedAt;
      recordQoe(playback.qoe.attempts, attempt);
      playback.presentationAttempt = null;
      return true;
    }

    function recordQoe(records, value) {
      records.push(value);
      if (records.length > 100) records.shift();
    }

    function qoePresentationEligible(video) {
      return playback.playIntent && document.visibilityState !== 'hidden' &&
        !video?.seeking && !playback.seekCommitTimer && !playback.requestController;
    }

    function qoePlayingEligible(video) {
      return playback.hasPresentedFrame && !playback.presentationAttempt && qoePresentationEligible(video);
    }

    function hasQoeData() {
      return playback.qoe.attempts.length > 0 || playback.qoe.playbackStallCount > 0 ||
        playback.qoe.intendedPlayingMs > 0 || playback.qoe.intendedPlayingStartedAt > 0;
    }

    function updateQoePlayingTime(now = performance.now()) {
      const eligible = qoePlayingEligible($('video'));
      if (!eligible && playback.qoe.intendedPlayingStartedAt) {
        playback.qoe.intendedPlayingMs += now - playback.qoe.intendedPlayingStartedAt;
        playback.qoe.intendedPlayingStartedAt = 0;
      } else if (eligible && !playback.qoe.intendedPlayingStartedAt) {
        playback.qoe.intendedPlayingStartedAt = now;
      }
    }

    function publishQoeSummary(now = performance.now()) {
      updateQoePlayingTime(now);
      if (!hasQoeData()) return;
      const remaining = playback.qoe.lastPublishedAt ? 5000 - (now - playback.qoe.lastPublishedAt) : 0;
      if (remaining > 0) {
        if (!playback.qoe.publishTimer) {
          playback.qoe.publishTimer = setTimeout(() => {
            playback.qoe.publishTimer = null;
            publishQoeSummary();
          }, remaining);
        }
        return;
      }
      playback.qoe.lastPublishedAt = now;
      const intendedPlayingMs = playback.qoe.intendedPlayingMs +
        (playback.qoe.intendedPlayingStartedAt ? now - playback.qoe.intendedPlayingStartedAt : 0);
      const denominator = Math.max(1, intendedPlayingMs);
      window.dispatchEvent(new CustomEvent('cinemator:qoe', { detail: {
        attempts: playback.qoe.attempts.slice(),
        playbackStallCount: playback.qoe.playbackStallCount,
        playbackStallDurationSeconds: playback.qoe.playbackStallDurationMs / 1000,
        playbackStallRatio: playback.qoe.playbackStallDurationMs / denominator,
        rebufferCount: playback.qoe.rebufferCount,
        rebufferDurationSeconds: playback.qoe.rebufferDurationMs / 1000,
        rebufferRatio: playback.qoe.rebufferDurationMs / denominator,
        stallFreeSession: playback.qoe.playbackStallCount === 0,
        intendedPlayingSeconds: intendedPlayingMs / 1000,
        stalls: playback.qoe.stalls.slice(),
      } }));
    }

    function observePresentedFrame(now, mediaTime) {
      if (!playback.acceptPresentedMediaTime(mediaTime)) return;
      playback.lastPresentedFrameAt = now;
      playback.lastActivityAt = Date.now();
      playback.hasPresentedFrame = true;
      playback.presentationRecoveryAttempted = false;
      playback.presentedFrameCount++;
      const video = $('video');
      syncPlaybackControls(video);
      if (playback.presentationAttempt && qoePresentationEligible(video)) {
        finishPresentationAttempt('presented', now);
      }
      const presentedForPlayback = qoePlayingEligible(video);
      if (playback.stallStartedAt && presentedForPlayback) {
        finishPlaybackStall(now);
        if (!playback.requestController) {
          removeWarning();
          showPlaybackNotice();
        }
      }
      publishQoeSummary(now);
    }

    function finishPlaybackStall(now = performance.now()) {
      if (!playback.stallStartedAt) return;
      const durationMs = now - playback.stallStartedAt;
      playback.qoe.playbackStallDurationMs += durationMs;
      if (playback.stallWasUnderrun) playback.qoe.rebufferDurationMs += durationMs;
      playback.stallStartedAt = 0;
      playback.stallWasUnderrun = false;
    }

    function startFrameObservation(video, signal) {
      playback.lastPresentedFrameAt = performance.now();
      playback.hasPresentedFrame = false;
      playback.presentedFrameCount = 0;
      playback.stallStartedAt = 0;
      let decodedFrames = 0;
      let lastUnpresentedMediaTime = Number(video.currentTime) || 0;
      let unpresentedClockStartedAt = 0;
      let firstFrameFailureHandled = false;
      const observe = (now, metadata) => observePresentedFrame(
        Number.isFinite(now) ? now : performance.now(),
        Number.isFinite(metadata?.mediaTime) ? metadata.mediaTime : Number(video.currentTime),
      );
      if (video.requestVideoFrameCallback) {
        const next = () => {
          playback.frameCallback = video.requestVideoFrameCallback((now, metadata) => {
            if (signal.aborted) return;
            observe(now, metadata);
            next();
          });
        };
        next();
      } else if (video.getVideoPlaybackQuality) {
        decodedFrames = Number(video.getVideoPlaybackQuality()?.totalVideoFrames) || 0;
      }

      const frameRate = Number(playback.mediaInfo?.frameRate);
      const stallThresholdMs = Math.max(500, frameRate > 0 ? 2000 / frameRate : 500);
      playback.frameTimer = setInterval(() => {
        if (signal.aborted) return;
        if (!video.requestVideoFrameCallback && video.getVideoPlaybackQuality) {
          const nextFrames = Number(video.getVideoPlaybackQuality()?.totalVideoFrames) || 0;
          if (nextFrames > decodedFrames) observe(performance.now());
          decodedFrames = nextFrames;
        }
        if (!playback.playIntent || document.visibilityState === 'hidden' || video.seeking || playback.seekCommitTimer || playback.requestController) return;
        const now = performance.now();
        updateQoePlayingTime(now);
        const minimumFrames = frameRate > 0 ? 1 : 2;
        if (playback.presentedFrameCount < minimumFrames) {
          const mediaTime = Number(video.currentTime) || 0;
          if (mediaTime > lastUnpresentedMediaTime + 0.01) {
            if (!unpresentedClockStartedAt) unpresentedClockStartedAt = now;
            lastUnpresentedMediaTime = mediaTime;
          }
          if (!firstFrameFailureHandled && unpresentedClockStartedAt &&
            now - unpresentedClockStartedAt >= Math.max(1000, stallThresholdMs)) {
            firstFrameFailureHandled = true;
            showWarning(
              'Video is not rendering',
              'The media clock is advancing without decoded video frames. Switching to the compatibility fallback.',
            );
            retryStartupWithCompatibilityFallback('The original stream did not produce decoded video frames; using the compatibility fallback.');
          }
          return;
        }
        if (now - playback.lastPresentedFrameAt < stallThresholdMs || playback.stallStartedAt) return;
        playback.stallStartedAt = now;
        playback.stallWasUnderrun = !playback.timeline.buffered(mediaBufferedRanges(video), video.currentTime);
        playback.qoe.playbackStallCount++;
        if (playback.stallWasUnderrun) playback.qoe.rebufferCount++;
        const ranges = mediaBufferedRanges(video);
        const currentRange = ranges.find(range => range.start <= video.currentTime && range.end > video.currentTime);
        recordQoe(playback.qoe.stalls, {
          at: playback.timeline.sourceTime(video.currentTime),
          bufferedAhead: currentRange ? currentRange.end - video.currentTime : 0,
          underrun: playback.stallWasUnderrun,
        });
        publishQoeSummary(now);
        showWarning(
          `Playback stalled at ${formatPlaybackTime(playback.timeline.sourceTime(video.currentTime))}`,
          'Video frames stopped advancing. Playback is waiting in place for the next frame.',
          playback.stallWasUnderrun ? 'The forward buffer is empty; the timeline and current position will not be replaced.' : 'Buffered media is present; the player will not change position automatically.',
        );
        if (playback.stallWasUnderrun) {
          beginHlsStatusPolling(playback.timeline.sourceTime(video.currentTime));
        }
      }, 250);
      signal.addEventListener('abort', () => {
        if (playback.frameCallback !== null && video.cancelVideoFrameCallback) {
          video.cancelVideoFrameCallback(playback.frameCallback);
        }
        playback.frameCallback = null;
        if (playback.frameTimer) clearInterval(playback.frameTimer);
        playback.frameTimer = null;
      }, { once: true });
    }

    function playbackControlTime(video) {
      const committed = playback.restorationMediaTime();
      return Number.isFinite(committed) ? committed : Math.max(0, Number(video?.currentTime) || 0);
    }

    function syncPlaybackControls(video = $('video')) {
      if (!video) return;
      const duration = Math.max(0, Number(playback.mediaInfo?.duration) || Number(video.duration) || 0);
      const currentTime = Math.min(duration || Number.MAX_SAFE_INTEGER, playbackControlTime(video));
      const timeline = $('seekTimeline');
      if (timeline) {
        timeline.max = String(duration);
        timeline.value = String(currentTime);
        timeline.disabled = duration <= 0 || !playback.mediaSeekable;
        timeline.setAttribute(
          'aria-valuetext',
          `${formatPlaybackTime(currentTime)} of ${formatPlaybackTime(duration)}`,
        );
      }
      if ($('currentTimeLabel')) $('currentTimeLabel').value = formatPlaybackTime(currentTime);
      if ($('durationLabel')) $('durationLabel').value = formatPlaybackTime(duration);
      if ($('playPauseBtn')) {
        $('playPauseBtn').textContent = playback.playIntent ? 'Pause' : 'Resume';
        $('playPauseBtn').setAttribute('aria-label', playback.playIntent ? 'Pause playback' : 'Resume playback');
      }
      if ($('muteBtn')) {
        const muted = video.muted || video.volume === 0;
        $('muteBtn').textContent = muted ? 'Unmute' : 'Mute';
        $('muteBtn').setAttribute('aria-label', muted ? 'Unmute' : 'Mute');
      }
      if ($('volumeControl')) $('volumeControl').value = String(video.volume);
    }

    function syncFullscreenControl() {
      const button = $('fullscreenBtn');
      if (!button) return;
      const active = Boolean(document.fullscreenElement);
      button.textContent = active ? 'Exit full screen' : 'Full screen';
      button.setAttribute('aria-label', active ? 'Exit full screen' : 'Enter full screen');
    }

    function commitColdSeek(target) {
      const duration = Number(playback.mediaInfo?.duration);
      if (duration > 0) {
        startPlayback(Math.min(target, Math.max(0, duration - 0.001)), { keepPlayerVisible: true });
      } else {
        beginHlsStatusPolling(target, true);
      }
    }

    function requestUserSeek(mediaTime, video = $('video')) {
      const duration = Number(playback.mediaInfo?.duration);
      const upperBound = duration > 0 ? Math.max(0, duration - 0.001) : Number.MAX_SAFE_INTEGER;
      const nextMediaTime = Math.min(upperBound, Math.max(0, Number(mediaTime) || 0));
      const target = playback.timeline.sourceTime(nextMediaTime);
      playback.attaching = false;
      playback.commitSeekPosition(nextMediaTime);
      video.currentTime = nextMediaTime;
      syncPlaybackControls(video);
      const retained = playback.timeline.contains(
        target,
        mediaBufferedRanges(video),
        playback.hls?.latestLevelDetails?.fragments,
      );
      if (playback.requestController) {
        if (playback.isRequesting(target)) return;
        playback.cancelRequest();
        if (retained) {
          playback.cancelSeekCommit();
          beginPresentationAttempt('seek', target, 'retained');
          if (playback.hls?.startLoad) playback.hls.startLoad(playback.timeline.hlsTime(target));
          playback.lastActivityAt = Date.now();
          removeWarning();
          showPlaybackNotice();
          beginHlsStatusPolling(target);
        } else {
          playback.scheduleSeekCommit(target, commitColdSeek);
        }
        return;
      }
      if (retained) {
        playback.cancelSeekCommit();
        beginPresentationAttempt('seek', target, 'retained');
        beginHlsStatusPolling(target, true);
        return;
      }
      playback.scheduleSeekCommit(target, commitColdSeek);
    }

    function attachPlaybackControls() {
      $('seekTimeline')?.addEventListener('input', event => {
        requestUserSeek(Number(event.currentTarget.value));
      });
      $('playPauseBtn')?.addEventListener('click', () => {
        const video = $('video');
        if (playback.playIntent) {
          playback.playIntent = false;
          if (!video.paused) {
            video.pause();
          } else {
            finishPlaybackStall();
            publishQoeSummary();
            syncPlaybackControls(video);
          }
          return;
        }
        playback.playIntent = true;
        syncPlaybackControls(video);
        if (!playback.hls || !video.paused) return;
        video.play().catch(error => {
          if (error?.name !== 'NotAllowedError') return;
          playback.playIntent = false;
          playback.autoplayBlocked = true;
          syncPlaybackControls(video);
          showMsg('playerMsg', 'Video is ready. Press Play to begin.', false, true);
        });
      });
      $('muteBtn')?.addEventListener('click', () => {
        const video = $('video');
        video.muted = !video.muted;
        syncPlaybackControls(video);
      });
      $('volumeControl')?.addEventListener('input', event => {
        const video = $('video');
        video.volume = Number(event.currentTarget.value);
        if (video.volume > 0) video.muted = false;
        syncPlaybackControls(video);
      });
      $('fullscreenBtn')?.addEventListener('click', async () => {
        try {
          if (document.fullscreenElement) {
            await document.exitFullscreen?.();
          } else {
            await $('player-block')?.requestFullscreen?.();
          }
        } catch (_) {
          showMsg('playerMsg', 'Full-screen mode is unavailable in this browser.', true);
        }
      });
      document.addEventListener('fullscreenchange', syncFullscreenControl);
    }

    attachPlaybackControls();

    function attachPlaybackRecovery(video, signal) {
      const options = { signal };
      video.addEventListener('play', () => {
        playback.playIntent = true;
        if (playback.autoplayBlocked) {
          playback.autoplayBlocked = false;
          beginPresentationAttempt('autoplay_resume', playback.timeline.sourceTime(video.currentTime));
        }
        updateQoePlayingTime();
        syncPlaybackControls(video);
      }, options);
      video.addEventListener('pause', () => {
        finishPlaybackStall();
        playback.playIntent = false;
        publishQoeSummary();
        syncPlaybackControls(video);
      }, options);
      video.addEventListener('playing', () => {
        playback.timeline.anchor(video.currentTime);
        showPlaybackNotice();
        removeWarning();
        syncPlaybackControls(video);
      }, options);
      video.addEventListener('timeupdate', () => syncPlaybackControls(video), options);
      video.addEventListener('durationchange', () => syncPlaybackControls(video), options);
      video.addEventListener('volumechange', () => syncPlaybackControls(video), options);
      video.addEventListener('seeking', () => {
        finishPlaybackStall();
        updateQoePlayingTime();
        // User seeks enter through the application-owned timeline. Any other
        // media seek belongs to attachment, playlist refresh, or decoder
        // recovery and cannot replace the committed application position.
        const protectedTime = playback.restorationMediaTime();
        const nextTime = Number(video.currentTime);
        if (!playback.attaching && !playback.restoringPlayhead &&
          Number.isFinite(protectedTime) && Number.isFinite(nextTime) &&
          Math.abs(nextTime - protectedTime) > 0.05) {
          playback.restoringPlayhead = true;
          video.currentTime = protectedTime;
          queueMicrotask(() => { playback.restoringPlayhead = false; });
        }
        syncPlaybackControls(video);
      }, options);
      video.addEventListener('seeked', () => {
        updateQoePlayingTime();
        if (playback.seekCommitTimer || playback.requestController) return;
        if (video.readyState >= 3) {
          removeWarning();
        } else {
          beginHlsStatusPolling(playback.timeline.sourceTime(video.currentTime));
        }
      }, options);
      document.addEventListener('visibilitychange', () => {
        if (document.visibilityState === 'hidden') finishPlaybackStall();
        publishQoeSummary();
      }, options);
      signal.addEventListener('abort', () => playback.cancelSeekCommit(), { once: true });
      video.addEventListener('waiting', () => {
        if (playback.seekCommitTimer || playback.requestController) return;
        beginHlsStatusPolling(playback.timeline.sourceTime(video.currentTime));
      }, options);
      video.addEventListener('stalled', () => {
        if (playback.seekCommitTimer || playback.requestController) return;
        beginHlsStatusPolling(playback.timeline.sourceTime(video.currentTime));
      }, options);
      video.addEventListener('error', () => {
        playback.attaching = false;
        if (playback.usesPassthrough && playback.nativeRetries === 0) {
          playback.nativeRetries++;
          if (retryStartupWithCompatibilityFallback('The browser rejected the original stream; using the compatibility fallback.')) return;
        }
        showPlaybackError('Native media error');
      }, options);
      syncPlaybackControls(video);
      syncFullscreenControl();
    }

    function hlsPlaylistLoadPolicy() {
      return {
        default: {
          maxTimeToFirstByteMs: 60 * 1000,
          maxLoadTimeMs: 2 * 60 * 1000,
          timeoutRetry: {
            maxNumRetry: 2,
            retryDelayMs: 1000,
            maxRetryDelayMs: 4000,
          },
          errorRetry: {
            maxNumRetry: 6,
            retryDelayMs: 1000,
            maxRetryDelayMs: 8000,
          },
        },
      };
    }

    function playHls(src, enableSubtitles, streamConfig = {}) {
      const video = $('video');
      if (playback.events) playback.events.abort();
      playback.events = new AbortController();
      playback.attaching = true;
      const eventOptions = { signal: playback.events.signal };
      playback.lastActivityAt = Date.now();
      attachPlaybackRecovery(video, playback.events.signal);
      startFrameObservation(video, playback.events.signal);

      const reapplySubtitleDelay = () => applySubtitleDelay(video, subtitleDelay);
      if (enableSubtitles) {
        watchSubtitleUpdates(video, reapplySubtitleDelay, playback.events.signal);
      }

      let mainFragBuffered = false;
      let playbackStartRequested = false;
      function currentPositionBuffered() {
        const ranges = mediaBufferedRanges(video);
        if (playback.timeline.buffered(ranges, video.currentTime)) return true;
        if (!playback.attaching || playback.hasPresentedFrame) return false;
        const nextRange = ranges.find(range => range.start > video.currentTime && range.start - video.currentTime <= 0.25);
        if (!nextRange) return false;
        video.currentTime = nextRange.start;
        playback.protectedMediaTime = nextRange.start;
        return true;
      }
      function markStreamActive() {
        mainFragBuffered = currentPositionBuffered();
        if (!mainFragBuffered) return false;
        playback.attaching = false;
        playback.timeline.anchor(video.currentTime);
        playback.nativeRetries = 0;
        playback.missingStreamRecoveryAttempted = false;
        playback.lastActivityAt = Date.now();
        showPlaybackNotice();
        removeWarning();
        return true;
      }
      function markPlaybackProgress() {
        playback.lastActivityAt = Date.now();
      }
      function requestPlaybackStart() {
        if (!playback.playIntent || playbackStartRequested || !video.paused) return;
        playbackStartRequested = true;
        video.play().catch(error => {
          if (error?.name === 'NotAllowedError') {
            playback.playIntent = false;
            playback.autoplayBlocked = true;
            syncPlaybackControls(video);
            const now = performance.now();
            if (finishPresentationAttempt('autoplay_blocked', now)) {
              publishQoeSummary(now);
            }
            removeWarning();
            showMsg('playerMsg', 'Video is ready. Press Play to begin.', false, true);
          } else {
            playbackStartRequested = false;
          }
        });
      }
      video.addEventListener('seeking', () => {
        mainFragBuffered = currentPositionBuffered();
      }, eventOptions);

      if (window.Hls && Hls.isSupported()) {
        const segmentSeconds = Math.max(1, Number(streamConfig.segmentSeconds) || 2);
        const byteCeiling = 128 * 1024 * 1024;
        const bitrate = Math.max(1, Number(streamConfig.bitrate) || 8_000_000);
        const byteBoundSeconds = Math.max(segmentSeconds * 2, byteCeiling * 8 / bitrate);
        const targetBuffer = Math.min(30, byteBoundSeconds);
        const highBuffer = Math.max(targetBuffer, Math.min(60, byteBoundSeconds));
        let deliveryRatio = 0;
        let deliveryJitter = 0;
        let deliverySamples = 0;
        const duration = Number(streamConfig.duration);
        const sourceStart = Math.max(0, Number(streamConfig.sourceStart) || 0);
        const presentationStart = playback.timeline.configure({
          sourceStart,
          duration,
          presentationOrigin: streamConfig.presentationOrigin,
          seekable: playback.mediaSeekable,
        });
        playback.protectedMediaTime = playback.timeline.absolute ? sourceStart : null;
        playback.hls = new Hls({
          autoStartLoad: false,
          timelineOffset: playback.timeline.absolute ? presentationStart : undefined,
          startPosition: playback.timeline.absolute ? playback.timeline.hlsTime(sourceStart) : -1,
          initialLiveManifestSize: 1,
          liveSyncMode: 'buffered',
          liveSyncDurationCount: 1,
          liveMaxLatencyDurationCount: 1_000_000,
          maxLiveSyncPlaybackRate: 1,
          maxBufferHole: 0,
          nudgeMaxRetry: 0,
          nudgeOnVideoHole: false,
          maxBufferLength: targetBuffer,
          maxMaxBufferLength: highBuffer,
          maxBufferSize: byteCeiling,
          backBufferLength: Math.min(60, byteBoundSeconds),
          fragLoadPolicy: {
            default: {
              maxTimeToFirstByteMs: 60 * 1000,
              maxLoadTimeMs: 2 * 60 * 1000,
              timeoutRetry: {
                maxNumRetry: 2,
                retryDelayMs: 0,
                maxRetryDelayMs: 0,
              },
              errorRetry: {
                maxNumRetry: 6,
                retryDelayMs: 1000,
                maxRetryDelayMs: 8000,
              },
            },
          },
          manifestLoadPolicy: hlsPlaylistLoadPolicy(),
          playlistLoadPolicy: hlsPlaylistLoadPolicy(),
        });
        playback.hls.on(Hls.Events.MANIFEST_PARSED, () => {
          playback.lastActivityAt = Date.now();
          playback.hls.startLoad(playback.timeline.absolute
            ? 0
            : -1);
          if (enableSubtitles && playback.hls.subtitleTracks && playback.hls.subtitleTracks.length > 0) {
            playback.hls.subtitleTrack = 0;
            playback.hls.subtitleDisplay = true;
            showSubtitleTextTracks(video);
            reapplySubtitleDelay();
          }
        });
        playback.hls.on(Hls.Events.MEDIA_ATTACHED, (evt, data) => {
          const mediaSource = data?.mediaSource;
          if (!playback.timeline.absolute || mediaSource?.readyState !== 'open') return;
          try {
            mediaSource.duration = duration;
          } catch (_) {
            // hls.js reapplies the override when level details arrive.
          }
        });
        if (enableSubtitles) {
          playback.hls.on(Hls.Events.SUBTITLE_TRACK_LOADED, () => {
            showSubtitleTextTracks(video);
            reapplySubtitleDelay();
          });
          playback.hls.on(Hls.Events.SUBTITLE_TRACK_SWITCH, () => {
            showSubtitleTextTracks(video);
            reapplySubtitleDelay();
          });
        }
        playback.hls.on(Hls.Events.FRAG_LOADED, (evt, data) => {
          markPlaybackProgress();
          const frag = data?.frag;
          if (!frag || frag.type !== 'main') return;
          const started = Number(frag.stats?.loading?.start);
          const ended = Number(frag.stats?.loading?.end);
          const mediaSeconds = Number(frag.duration);
          if (!Number.isFinite(started) || !Number.isFinite(ended) || ended <= started || mediaSeconds <= 0) return;
          const sample = Math.min(4, (ended - started) / 1000 / mediaSeconds);
          if (deliverySamples === 0) {
            deliveryRatio = sample;
          } else {
            deliveryJitter = 0.75 * deliveryJitter + 0.25 * Math.abs(sample - deliveryRatio);
            deliveryRatio = 0.75 * deliveryRatio + 0.25 * sample;
          }
          deliverySamples++;
          playback.hls.config.maxBufferLength = deliveryRatio + deliveryJitter >= 0.75
            ? highBuffer
            : targetBuffer;
        });
        playback.hls.on(Hls.Events.FRAG_BUFFERED, (evt, data) => {
          if (!data?.frag || data.frag.type === 'main') {
            if (!markStreamActive()) return;
            requestPlaybackStart();
          }
        });
        playback.hls.on(Hls.Events.ERROR, (evt, data) => {
          if (enableSubtitles && data?.frag?.type === 'subtitle') {
            if (data.fatal) {
              playback.attaching = false;
              video.pause();
              showPlaybackError('The selected subtitles could not be loaded');
            }
            return;
          }
          const responseCode = Number(data?.response?.code || data?.response?.status || 0);
          if (responseCode === 409) {
            if (recoverChangedHlsPresentation(sourceStart)) return;
            playback.attaching = false;
            showPlaybackError('The stream presentation changed before the player loaded it');
            return;
          }
          const levelEmpty = data.details === 'levelEmptyError' ||
            (Hls.ErrorDetails && data.details === Hls.ErrorDetails.LEVEL_EMPTY_ERROR);
          if (levelEmpty && (mainFragBuffered || Date.now() - playback.lastActivityAt < 8000)) {
            return;
          }
          if (data.fatal) {
            playback.attaching = false;
            const mediaFailure = data.type === Hls.ErrorTypes?.MEDIA_ERROR || /codec|buffer|media/i.test(data.details || '');
            if (mediaFailure) {
              if (retryStartupWithCompatibilityFallback('The browser rejected the original stream; using the compatibility fallback.')) return;
            }
            showPlaybackError(data.details || 'Fatal error');
          }
        });
        playback.hls.loadSource(src);
        playback.hls.attachMedia(playback.timeline.absolute
          ? { media: video, overrides: { duration } }
          : video);
      } else if (video.canPlayType('application/vnd.apple.mpegurl')) {
        video.src = src;
        video.addEventListener('loadedmetadata', () => { playback.lastActivityAt = Date.now(); }, { once: true, signal: playback.events.signal });
        video.addEventListener('canplay', () => {
          if (markStreamActive()) requestPlaybackStart();
        }, { once: true, signal: playback.events.signal });
        video.addEventListener('playing', markStreamActive, eventOptions);
        video.addEventListener('progress', markPlaybackProgress, eventOptions);
        video.addEventListener('timeupdate', markPlaybackProgress, eventOptions);
        video.addEventListener('loadeddata', () => {
          markStreamActive();
          if (enableSubtitles) showSubtitleTextTracks(video);
          reapplySubtitleDelay();
        }, { once: true, signal: playback.events.signal });
      } else {
        removeWarning();
        const message = window.Hls
          ? 'Your browser does not support HLS.'
          : 'The HLS player library could not load. Check access to cdn.jsdelivr.net and reload the page.';
        showMsg('playerMsg', message, true);
      }
    }

    function updateDelayDisplay() {
      const el = $('subDelayValue');
      if (el) el.textContent = `${subtitleDelay.toFixed(1)}s`;
    }
    function changeDelay(delta) {
      subtitleDelay = Math.max(-10, Math.min(10, subtitleDelay + delta));
      localStorage.setItem('subtitle-delay', subtitleDelay.toString());
      updateDelayDisplay();
      applySubtitleDelay($('video'), subtitleDelay);
      if (playback.hls && playback.hls.subtitleTracks && playback.hls.subtitleTracks.length > 0) {
        playback.hls.subtitleDisplay = true;
      }
    }
    updateDelayDisplay();
    const minusBtn = $('subDelayMinus');
    const plusBtn = $('subDelayPlus');
    if (minusBtn) minusBtn.onclick = () => changeDelay(-0.5);
    if (plusBtn) plusBtn.onclick = () => changeDelay(0.5);

    const mediaInfoDialog = $('mediaInfoDialog');
    const mediaInfoButton = $('mediaInfoBtn');
    const mediaInfoClose = $('mediaInfoClose');
    if (mediaInfoButton && mediaInfoDialog) {
      mediaInfoButton.onclick = () => {
        renderMediaInfo();
        mediaInfoDialog.showModal();
      };
      mediaInfoDialog.addEventListener('click', event => {
        if (event.target === mediaInfoDialog) mediaInfoDialog.close();
      });
    }
    if (mediaInfoClose && mediaInfoDialog) {
      mediaInfoClose.onclick = () => mediaInfoDialog.close();
    }

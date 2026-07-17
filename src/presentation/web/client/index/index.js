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
    if (logoutBtn) {
      window.fetch('/api/auth/status', { cache: 'no-store' })
        .then(response => response.ok ? response.json() : null)
        .then(status => {
          logoutBtn.hidden = !status?.enabled;
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

    let hls = null;
    let subtitleDelay = parseFloat(localStorage.getItem('subtitle-delay') || '0');
    let msgTimeout = null;
    let subtitleWaitTimer = null;
    let playbackRecoveryTimer = null;
    let playbackRecoveryInFlight = false;
    let lastPlaybackRecoveryAt = 0;
    let lastStreamActivityAt = 0;
    let forcedRecoveryAttempts = 0;
    let downloadCatalog = [];
    let downloadsLoading = false;
    let downloadsRefreshQueued = false;
    let downloadEvents = null;
    let downloadFallbackPolling = false;
    let downloadPollingTimer = null;
    let openExtendDownloadID = null;
    let requestSeq = 0;
    let flowRequestController = null;
    let activeStreamDir = null;
    let currentMediaSeekable = true;
    let currentMediaInfo = null;
    let currentPlaybackMode = '';
    let currentPlaybackDecision = '';
    let playbackUsesPassthrough = false;
    let forceTranscodePlayback = false;
    let nativePassthroughRetries = 0;
    let playbackNotice = '';
    let hlsStatusTimer = null;
    let hlsStatusController = null;
    let hlsStatusSeq = 0;
    const idleRecoveryMs = 12 * 60 * 1000;
    const recoveryThrottleMs = 5000;
    const stallRecoveryDelayMs = 3500;
    const maxForcedRecoveryAttempts = 3;
    const downloadFallbackInitialMs = 5000;
    const downloadFallbackPollingMs = 30000;
    const extendOptions = [
      { days: 1, label: '1 day' },
      { days: 7, label: '7 days' },
      { days: 30, label: '30 days' },
    ];
    const isStale = id => id !== requestSeq;
    function cancelFlowRequest() {
      requestSeq++;
      if (flowRequestController) {
        flowRequestController.abort();
        flowRequestController = null;
      }
    }
    function beginFlowRequest() {
      cancelFlowRequest();
      const request = {
        id: requestSeq,
        controller: new AbortController(),
      };
      request.signal = request.controller.signal;
      flowRequestController = request.controller;
      return request;
    }
    function finishFlowRequest(request) {
      if (flowRequestController === request.controller) {
        flowRequestController = null;
      }
    }
    function destroyVideoAndHls({ resetLayout = true } = {}) {
      if (hls) { hls.destroy(); hls = null; }
      stopHlsStatusPolling();
      activeStreamDir = null;
      const mediaDialog = $('mediaInfoDialog');
      if (mediaDialog?.open) mediaDialog.close();
      if (playbackRecoveryTimer) {
        clearTimeout(playbackRecoveryTimer);
        playbackRecoveryTimer = null;
      }
      if (resetLayout) document.body.classList.remove('has-player');
      const oldVideo = $('video');
      const newVideo = oldVideo.cloneNode(false);
      oldVideo.parentNode.replaceChild(newVideo, oldVideo);
      newVideo.id = 'video';
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
      stopHlsStatusPolling();
      clearSubtitleWait();
      $('warnMsg').hidden = true;
    }

    function stopHlsStatusPolling() {
      hlsStatusSeq++;
      if (hlsStatusTimer) {
        clearTimeout(hlsStatusTimer);
        hlsStatusTimer = null;
      }
      if (hlsStatusController) {
        hlsStatusController.abort();
        hlsStatusController = null;
      }
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
      if (status.mode && status.mode !== currentPlaybackMode) {
        if (status.mode === 'transcode' && playbackUsesPassthrough) {
          playbackUsesPassthrough = false;
          currentPlaybackDecision = 'The source could not be remuxed safely; the server switched to the compatibility fallback.';
        }
        currentPlaybackMode = status.mode;
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
          'There are no active peers for the required torrent pieces. Peer discovery is still running.',
          `${peerText}${suffix}`,
        );
        return;
      }
      if (status.phase === 'stalled') {
        showWarning(
          `Stalled at ${formatPlaybackTime(target)}`,
          status.message || 'Connected peers and the transcoder have not produced data recently.',
          `${peerText}${suffix} · still retrying`,
        );
        return;
      }
      if (status.phase === 'error') {
        showWarning(
          `Stream failed at ${formatPlaybackTime(target)}`,
          status.message || 'The server could not generate the requested segment.',
          `Preparation stopped · ${peerText}${suffix}`,
        );
        return;
      }
      if (status.phase === 'ready') {
        showWarning(
          `Position ${formatPlaybackTime(target)} is ready`,
          'The prepared segment is being sent to the player.',
          `${formatBytes(status.bytesRead || 0)} read · ${peerText}${suffix}`,
        );
        return;
      }
      if (status.phase === 'waiting') {
        showWarning(
          `Starting ${formatPlaybackTime(target)}`,
          'Media information is ready. Waiting for the player to request its first HLS segment.',
          `${peerText}${suffix}`,
        );
        return;
      }
      showWarning(
        `Preparing ${formatPlaybackTime(target)}`,
        `Downloading the required torrent pieces and ${preparation} only for the requested window.`,
        `${formatBytes(status.bytesRead || 0)} read · ${peerText}${suffix}`,
      );
    }

    function beginHlsStatusPolling(targetSeconds) {
      if (!activeStreamDir) return;
      stopHlsStatusPolling();
      const seq = hlsStatusSeq;
      showWarning(
        `Preparing ${formatPlaybackTime(targetSeconds)}`,
        'Checking the cache and requesting the required torrent pieces.',
        'Waiting for server status…',
      );
      let consecutiveFailures = 0;
      const poll = async () => {
        if (seq !== hlsStatusSeq || !activeStreamDir) return;
        const controller = new AbortController();
        hlsStatusController = controller;
        try {
          const statusURL = `/api/hls/status/${encodeURIComponent(activeStreamDir)}?target=${encodeURIComponent(targetSeconds)}`;
          const response = await apiFetch(statusURL, {
            cache: 'no-store',
            signal: controller.signal,
          }, 10000);
          if (seq !== hlsStatusSeq) return;
          if (!response.ok) {
            const error = new Error((await response.text()).trim() || `Status request failed (${response.status})`);
            error.status = response.status;
            throw error;
          }
          consecutiveFailures = 0;
          const status = await response.json();
          if (seq !== hlsStatusSeq) return;
          renderHlsStatus(status, targetSeconds);
        } catch (error) {
          if (error.name !== 'AbortError' && seq === hlsStatusSeq) {
            consecutiveFailures++;
            if (consecutiveFailures >= 3) {
              if (error.status === 404 && requestPlaybackRecovery('stream-worker-lost', true)) {
                return;
              }
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
          if (hlsStatusController === controller) hlsStatusController = null;
          if (seq === hlsStatusSeq) {
            hlsStatusTimer = setTimeout(poll, 1000);
          }
        }
      };
      poll();
    }

    function showPlaybackNotice() {
      showMsg('playerMsg', playbackNotice);
      if (playbackNotice) {
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
      const tracks = currentMediaInfo?.audioTracks || [];
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
      const info = currentMediaInfo;
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
      $('mediaPlaybackMode').textContent = modes[currentPlaybackMode] || 'Preparing';
      setMediaInfoRow('mediaDecisionRow', 'mediaDecision', currentPlaybackDecision);
    }

    function setMediaInfo(info = null) {
      currentMediaInfo = info;
      currentMediaSeekable = info?.seekable === true;
      currentPlaybackMode = '';
      currentPlaybackDecision = '';
      playbackUsesPassthrough = false;
      forceTranscodePlayback = false;
      nativePassthroughRetries = 0;
      playbackNotice = '';
      renderMediaInfo();
    }

    function useCompatibilityFallback(reason) {
      if (!playbackUsesPassthrough) return;
      forceTranscodePlayback = true;
      currentPlaybackDecision = reason || 'The original stream failed to decode; using the compatibility fallback.';
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
      if (!info.seekable) {
        return { supported: false, reason: 'The source has no stable duration, so playback uses progressive transcoding.' };
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
      cancelFlowRequest();
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
          cancelFlowRequest();
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
    startDownloadEvents();

    $('form').onsubmit = async e => {
      e.preventDefault();
      const magnet = $('magnet').value.trim();
      if (!magnet) {
        showMsg('magnetMsg', 'Magnet link required', true);
        return;
      }
      const request = beginFlowRequest();
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
        if (isStale(requestId)) return;
        if (!files.length) throw new Error('No playable files found in torrent');
        setFileList(files);
        $('step-files').style.display = '';
        showMsg('magnetMsg', '');
      } catch (e) {
        if (e.name === 'AbortError') return;
        if (isStale(requestId)) return;
        showMsg('magnetMsg', e.message || 'Error loading files', true);
        return;
      } finally {
        finishFlowRequest(request);
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
      const request = beginFlowRequest();
      const requestId = request.id;
      showMsg('fileMsg', 'Loading track info…', false, true);
      $('step-tracks').style.display = 'none';
      try {
        const res = await apiFetch(`/api/media/info?magnet=${encodeURIComponent(magnet)}&file=${idx}`, { signal: request.signal }, 60000);
        if (!res.ok) throw new Error((await res.text()).trim() || 'Could not load media info');
        const info = await res.json();
        if (isStale(requestId)) return;
        setMediaInfo(info);
        const notices = Array.isArray(info.warnings) ? [...info.warnings] : [];
        if (!currentMediaSeekable) {
          notices.unshift('Duration is unavailable for this format. Playback is progressive; seeking is limited to the discovered part.');
        }
        playbackNotice = notices.join(' ');

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
        if (isStale(requestId)) return;
        showMsg('fileMsg', e.message || 'Error loading tracks', true);
      } finally {
        finishFlowRequest(request);
      }
    };

    async function startPlayback(resumeTime = 0, { keepPlayerVisible = false } = {}) {
      const magnet = $('magnet').value.trim();
      const idx = $('filelist').value;
      const audio = $('audioSelect').value || '0';
      const subtitle = $('subtitleSelect').value || '-1';
      const subtitleSelected = parseInt(subtitle, 10) >= 0;
      if (!magnet || idx === undefined || idx === '') {
        showMsg('trackMsg', 'Select a file first', true);
        return;
      }
      const request = beginFlowRequest();
      const requestId = request.id;
      const wasPlaying = $('player-block').style.display !== 'none' || document.body.classList.contains('has-player');
      destroyVideoAndHls({ resetLayout: !wasPlaying });
      $('player-block').style.display = keepPlayerVisible ? '' : 'none';
      removeWarning();
      showWarning();
      showMsg('trackMsg', '');
      showMsg('playerMsg', keepPlayerVisible ? 'Restoring stream...' : '', false, keepPlayerVisible);
      lastStreamActivityAt = Date.now();
      if (!keepPlayerVisible) {
        forcedRecoveryAttempts = 0;
      }

      if (subtitleSelected) {
        clearSubtitleWait();
        subtitleWaitTimer = setTimeout(() => {
          setWarningExtra('Waiting for subtitles...');
        }, 8000);
      }
      try {
        const start = currentMediaSeekable && Number.isFinite(resumeTime) && resumeTime > 0 ? resumeTime : 0;
        const capability = await sourcePlaybackCapability(currentMediaInfo);
        if (isStale(requestId)) return;
        const forceTranscode = forceTranscodePlayback || !capability.supported;
        playbackUsesPassthrough = !forceTranscode;
        currentPlaybackMode = '';
        currentPlaybackDecision = forceTranscodePlayback
          ? 'The original stream failed to decode; using the compatibility fallback.'
          : capability.reason;
        renderMediaInfo();
        const resp = await apiFetch(`/api/hls/prepare?magnet=${encodeURIComponent(magnet)}&file=${idx}&audio=${audio}&subtitle=${subtitle}&start=${encodeURIComponent(start)}&transcode=${forceTranscode ? 1 : 0}`, {
          headers: { Accept: 'application/json' },
          signal: request.signal,
        }, 60000);
        if (!resp.ok) throw new Error((await resp.text()).trim() || 'Stream error');
        if (isStale(requestId)) return;
        const prepared = await resp.json();
        if (!prepared.playlist || !prepared.stream) throw new Error('Server returned an invalid stream descriptor');
        const playlistURL = new URL(prepared.playlist, window.location.href);
        activeStreamDir = prepared.stream;
        const m3u8 = playlistURL.pathname + '?t=' + Date.now();
        $('player-block').style.display = '';
        document.body.classList.add('has-player');
        showPlaybackNotice();
        beginHlsStatusPolling(start);
        const segmentSeconds = Number(prepared.segmentDurationSeconds) || 6;
        const windowSegments = Number(prepared.windowSegments) || 5;
        playHls(m3u8, subtitleSelected, start, { segmentSeconds, windowSegments });
      } catch (e) {
        if (e.name === 'AbortError') return;
        if (isStale(requestId)) return;
        removeWarning();
        const msg = e.message || 'Could not start stream';
        if (keepPlayerVisible) {
          showMsg('playerMsg', msg, true);
        } else {
          showMsg('trackMsg', msg, true);
        }
        return;
      } finally {
        finishFlowRequest(request);
      }
    }

    $('play').onclick = () => {
      forceTranscodePlayback = false;
      nativePassthroughRetries = 0;
      startPlayback(0);
    };
    ['audioSelect', 'subtitleSelect'].forEach(id => {
      const el = $(id);
      if (!el) return;
      el.addEventListener('change', () => {
        forceTranscodePlayback = false;
        nativePassthroughRetries = 0;
        renderMediaInfo();
        if ($('player-block').style.display !== 'none') {
          const t = $('video').currentTime || 0;
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

    function watchSubtitleUpdates(video, onUpdate) {
      if (!video || !video.textTracks) return;
      const tracks = video.textTracks;
      const attachTrack = track => {
        if (!track || !track.addEventListener) return;
        track.addEventListener('cuechange', onUpdate);
      };
      Array.from(tracks).forEach(attachTrack);
      if (tracks.addEventListener) {
        tracks.addEventListener('addtrack', e => {
          attachTrack(e.track);
          showSubtitleTextTracks(video);
          setTimeout(onUpdate, 0);
        });
      }
    }

    function requestPlaybackRecovery(reason, force = false) {
      const video = $('video');
      if (!video || $('player-block').style.display === 'none') return false;

      const now = Date.now();
      if (playbackRecoveryInFlight || now - lastPlaybackRecoveryAt < recoveryThrottleMs) {
        return false;
      }
      if (!force && now - lastStreamActivityAt < idleRecoveryMs) {
        return false;
      }

      playbackRecoveryInFlight = true;
      lastPlaybackRecoveryAt = now;
      if (force) {
        if (forcedRecoveryAttempts >= maxForcedRecoveryAttempts) {
          playbackRecoveryInFlight = false;
          return false;
        }
        forcedRecoveryAttempts++;
      }
      const resumeTime = Number.isFinite(video.currentTime) ? video.currentTime : 0;
      startPlayback(resumeTime, { keepPlayerVisible: true }).finally(() => {
        playbackRecoveryInFlight = false;
      });
      return true;
    }

    function schedulePlaybackRecovery(reason, force = false) {
      if (playbackRecoveryTimer) clearTimeout(playbackRecoveryTimer);
      playbackRecoveryTimer = setTimeout(() => {
        playbackRecoveryTimer = null;
        const video = $('video');
        if (!video || video.paused) return;
        if (force || video.readyState < 3) {
          requestPlaybackRecovery(reason, force);
        }
      }, stallRecoveryDelayMs);
    }

    function showPlaybackError(details) {
      removeWarning();
      showMsg('playerMsg', 'Playback error: ' + (details || 'Fatal error'), true);
    }

    function attachPlaybackRecovery(video) {
      video.addEventListener('play', () => requestPlaybackRecovery('play'));
      video.addEventListener('seeking', () => beginHlsStatusPolling(video.currentTime));
      video.addEventListener('seeked', () => {
        if (video.readyState >= 3) {
          removeWarning();
        } else {
          beginHlsStatusPolling(video.currentTime);
        }
      });
      video.addEventListener('waiting', () => {
        beginHlsStatusPolling(video.currentTime);
        schedulePlaybackRecovery('waiting');
      });
      video.addEventListener('stalled', () => {
        beginHlsStatusPolling(video.currentTime);
        schedulePlaybackRecovery('stalled');
      });
      video.addEventListener('error', () => {
        if (playbackUsesPassthrough && nativePassthroughRetries === 0) {
          nativePassthroughRetries++;
          if (requestPlaybackRecovery('native-direct-reload', true)) return;
        }
        useCompatibilityFallback('The browser rejected the original stream; using the compatibility fallback.');
        if (requestPlaybackRecovery('native-error', true)) return;
        showPlaybackError('Native media error');
      });
    }

    function longHlsLoadPolicy() {
      return {
        default: {
          maxTimeToFirstByteMs: 10 * 60 * 1000,
          maxLoadTimeMs: 10 * 60 * 1000,
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

    function playHls(src, enableSubtitles, resumeTime = 0, streamConfig = {}) {
      const video = $('video');
      video.style.opacity = 0;
      setTimeout(() => { video.style.opacity = 1; }, 120);
      lastStreamActivityAt = Date.now();
      attachPlaybackRecovery(video);

      const reapplySubtitleDelay = () => applySubtitleDelay(video, subtitleDelay);
      if (enableSubtitles) {
        watchSubtitleUpdates(video, reapplySubtitleDelay);
      }

      let mainFragBuffered = false;
      let subtitleFragLoaded = !enableSubtitles;
      let playlistReloadPosition = null;
      let playlistReloading = false;
      let playlistReloadAttempts = 0;
      function currentPositionBuffered() {
        const position = video.currentTime;
        for (let index = 0; index < video.buffered.length; index++) {
          if (position >= video.buffered.start(index) - 0.05 && position < video.buffered.end(index) + 0.05) {
            return true;
          }
        }
        return false;
      }
      function fragmentCoversCurrentPosition(fragment) {
        const start = Number(fragment?.start);
        const duration = Number(fragment?.duration);
        if (!Number.isFinite(start) || !Number.isFinite(duration)) return false;
        return video.currentTime >= start - 0.25 && video.currentTime < start + duration + 0.25;
      }
      function hideWarningOnce() {
        if (mainFragBuffered && subtitleFragLoaded) {
          removeWarning();
        }
      }
      function markStreamActive() {
        mainFragBuffered = currentPositionBuffered();
        if (!mainFragBuffered) return;
        nativePassthroughRetries = 0;
        lastStreamActivityAt = Date.now();
        forcedRecoveryAttempts = 0;
        playlistReloadAttempts = 0;
        showPlaybackNotice();
        hideWarningOnce();
      }
      function markPlaybackProgress() {
        lastStreamActivityAt = Date.now();
        forcedRecoveryAttempts = 0;
      }
      video.addEventListener('seeking', () => {
        mainFragBuffered = currentPositionBuffered();
        if (!mainFragBuffered && enableSubtitles) subtitleFragLoaded = false;
      });

      if (window.Hls && Hls.isSupported()) {
        const segmentSeconds = Math.max(1, Number(streamConfig.segmentSeconds) || 6);
        const windowSegments = Math.max(1, Number(streamConfig.windowSegments) || 5);
        const forwardBuffer = segmentSeconds * Math.min(windowSegments, 2);
        hls = new Hls({
          startPosition: resumeTime,
          maxBufferLength: forwardBuffer,
          maxMaxBufferLength: forwardBuffer,
          backBufferLength: segmentSeconds * 2,
          fragLoadPolicy: {
            default: {
              maxTimeToFirstByteMs: 10 * 60 * 1000,
              maxLoadTimeMs: 10 * 60 * 1000,
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
          manifestLoadPolicy: longHlsLoadPolicy(),
          playlistLoadPolicy: longHlsLoadPolicy(),
        });
        hls.loadSource(src);
        hls.attachMedia(video);

        hls.on(Hls.Events.MANIFEST_PARSED, () => {
          lastStreamActivityAt = Date.now();
          const target = playlistReloadPosition;
          playlistReloadPosition = null;
          playlistReloading = false;
          if (target !== null && target > 0) {
            video.currentTime = target;
          }
          if (enableSubtitles && hls.subtitleTracks && hls.subtitleTracks.length > 0) {
            hls.subtitleTrack = 0;
            hls.subtitleDisplay = true;
            showSubtitleTextTracks(video);
            reapplySubtitleDelay();
          }
        });
        if (enableSubtitles) {
          hls.on(Hls.Events.SUBTITLE_TRACK_LOADED, () => {
            showSubtitleTextTracks(video);
            reapplySubtitleDelay();
          });
          hls.on(Hls.Events.SUBTITLE_TRACK_SWITCH, () => {
            showSubtitleTextTracks(video);
            reapplySubtitleDelay();
          });
        }
        hls.on(Hls.Events.FRAG_LOADED, (evt, data) => {
          if (!data?.frag || data.frag.type === 'main') {
            markPlaybackProgress();
          } else {
            if (data.frag.type === 'subtitle' && fragmentCoversCurrentPosition(data.frag)) {
              subtitleFragLoaded = true;
              hideWarningOnce();
            }
            markPlaybackProgress();
          }
        });
        hls.on(Hls.Events.FRAG_BUFFERED, (evt, data) => {
          if (!data?.frag || data.frag.type === 'main') {
            markStreamActive();
          }
        });
        hls.on(Hls.Events.ERROR, (evt, data) => {
          if (enableSubtitles && data?.frag?.type === 'subtitle') {
            showMsg('playerMsg', `Subtitle error: ${data.details || 'could not load subtitles'}`, true);
            beginHlsStatusPolling(video.currentTime);
            return;
          }
          const responseCode = Number(data?.response?.code || data?.response?.status || 0);
          if (responseCode === 409) {
            if (!playlistReloading) {
              playlistReloadAttempts++;
              if (playlistReloadAttempts > 5) {
                showPlaybackError('The refreshed playlist still points to an unprepared media window');
                return;
              }
              playlistReloading = true;
              playlistReloadPosition = video.currentTime;
              const nextSource = new URL(src, window.location.href);
              nextSource.searchParams.set('reload', String(Date.now()));
              hls.loadSource(nextSource.href);
            }
            return;
          }
          const levelEmpty = data.details === 'levelEmptyError' ||
            (Hls.ErrorDetails && data.details === Hls.ErrorDetails.LEVEL_EMPTY_ERROR);
          if (levelEmpty && (mainFragBuffered || Date.now() - lastStreamActivityAt < 8000)) {
            return;
          }
          if (data.fatal) {
            const mediaFailure = data.type === Hls.ErrorTypes?.MEDIA_ERROR || /codec|buffer|media/i.test(data.details || '');
            if (mediaFailure) {
              useCompatibilityFallback('The browser rejected the original stream; using the compatibility fallback.');
            }
            if (requestPlaybackRecovery('hls-error', true)) return;
            showPlaybackError(data.details || 'Fatal error');
          }
        });
      } else if (video.canPlayType('application/vnd.apple.mpegurl')) {
        video.src = src;
        video.addEventListener('loadedmetadata', () => {
          video.currentTime = resumeTime;
        }, { once: true });
        video.addEventListener('canplay', markStreamActive, { once: true });
        video.addEventListener('playing', markStreamActive);
        video.addEventListener('progress', markPlaybackProgress);
        video.addEventListener('timeupdate', markPlaybackProgress);
        video.addEventListener('loadeddata', () => {
          markStreamActive();
          if (enableSubtitles) showSubtitleTextTracks(video);
          reapplySubtitleDelay();
        }, { once: true });
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
      if (hls && hls.subtitleTracks && hls.subtitleTracks.length > 0) {
        hls.subtitleDisplay = true;
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

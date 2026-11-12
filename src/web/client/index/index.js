import { t, setText, number, errorKey } from './i18n.js';

    // Main logic
    const $ = id => document.getElementById(id);
    async function apiFetch(input, init) {
      const response = await window.fetch(input, init);
      if (response.status === 401) {
        window.location.replace('/login');
        throw new Error('auth.sessionExpired');
      }
      return response;
    }

    async function responseError(response, fallback) {
      const error = new Error('errors.' + response.headers.get('X-Error-Code'));
      error.message = errorKey(error, fallback);
      error.detail = (await response.text()).trim();
      return error;
    }

    function showError(id, error, fallback) {
      showMsg(id, errorKey(error, fallback), true);
      renderErrorDetails($(id), error.detail);
    }

    function renderErrorDetails(container, diagnostic) {
      let details = container.querySelector(':scope > .error-details');
      if (!diagnostic) {
        details?.remove();
        return;
      }
      if (!details) {
        details = document.createElement('details');
        details.className = 'error-details';
        const summary = document.createElement('summary');
        setText(summary, 'common.details');
        details.append(summary, document.createElement('pre'));
        container.appendChild(details);
      }
      details.querySelector('pre').textContent = diagnostic;
    }

    const versionLabel = $('appVersion');
    if (versionLabel) {
      apiFetch('/api/version', { cache: 'no-store' })
        .then(response => response.ok ? response.json() : null)
        .then(info => {
          if (!info || typeof info.version !== 'string' || !info.version) return;
          versionLabel.textContent = `[${info.version}]`;
          versionLabel.hidden = false;
        })
        .catch(() => {});
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
          if (!response.ok) throw new Error('auth.signOutFailed');
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
    let lastStreamActivityAt = 0;
    let downloadCatalog = [];
    let downloadsLoading = false;
    let downloadsRefreshQueued = false;
    let lastDownloadCatalogRefreshAt = 0;
    let downloadEvents = null;
    let downloadPollingTimer = null;
    const downloadSamples = new Map();
    let progressUnavailable = false;
    let openDownloadMenuID = null;
    const downloadMenuResizeObserver = new ResizeObserver(() => {
      const menu = document.querySelector('.download-menu.open');
      if (menu) positionDownloadMenu(menu);
    });
    let activeDownloadID = null;
    let activeMagnet = '';
    let pendingResume = null;
    let tracksReady = false;
    let wantsPlayback = false;
    let retryTime = 0;
    let savePlaybackPosition = null;
    let requestSeq = 0;
    let flowRequestController = null;
    let filesRequestController = null;
    const downloadFallbackInitialMs = 5000;
    const downloadFallbackPollingMs = 30000;
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
        renderPreparationProgress();
        renderDownloads(downloadCatalog);
      }
    }
    function destroyVideoAndHls({ resetLayout = true } = {}) {
      savePlaybackPosition?.();
      savePlaybackPosition = null;
      if (hls) { hls.destroy(); hls = null; }
      if (resetLayout) document.body.classList.remove('has-player');
      const oldVideo = $('video');
      oldVideo.pause();
      oldVideo.removeAttribute('src');
      oldVideo.load();
      const newVideo = oldVideo.cloneNode(false);
      newVideo.style.opacity = '';
      oldVideo.parentNode.replaceChild(newVideo, oldVideo);
      newVideo.id = 'video';
    }
    const optionLabels = new WeakMap();
    let videoFiles = [];
    let selectedVideoIndex = null;
    let videoMenuState = null;
    const audioPreferences = new Map();
    function preferredAudioIndex(downloadID) {
      if (audioPreferences.has(downloadID)) return audioPreferences.get(downloadID);
      try {
        const index = JSON.parse(localStorage.getItem(`cinemator-audio:${downloadID}`));
        if (Number.isSafeInteger(index) && index >= 0) return index;
      } catch {}
      return null;
    }
    function setOptions(select, options) {
      select.replaceChildren(...options.map(({ value, label }) => {
        const option = new Option(label(), String(value));
        optionLabels.set(option, label);
        return option;
      }));
    }
    function fileStem(name) {
      return name.split(/[\\/]/).pop().replace(/\.(mkv|mp4|m4v|mov|avi|webm|ts|m2ts|mpg|mpeg|wmv|flv|ogv)$/i, '');
    }

    function setFileList(files) {
      videoFiles = files;
      selectedVideoIndex = files[0]?.index ?? null;
    }
    function showMsg(id, key, isErr=false, loader=false, params={}) {
      clearTimeout(msgTimeout);
      const el = $(id);
      el.textContent = '';
      if (loader) {
        const spinner = document.createElement('span');
        spinner.className = 'loader';
        el.appendChild(spinner);
      }
      if (key) {
        const text = document.createElement('span');
        setText(text, key, params);
        el.appendChild(text);
      }
      el.className = 'msg' + (isErr ? ' error' : '');
      const shouldAutoClear = key && !isErr && !loader;
      if (shouldAutoClear) {
        msgTimeout = setTimeout(() => { el.textContent = ''; }, 2200);
      }
    }
    function showWarning() {
      clearSubtitleWait();
      $('warnMsg').hidden = false;
      renderPreparationProgress();
      renderDownloads(downloadCatalog);
    }
    function setWarningExtra(msg) {
      const extra = document.getElementById('warn-extra');
      if (!extra) return;
      setText(extra, msg);
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
      clearSubtitleWait();
      $('warnMsg').hidden = true;
      renderDownloads(downloadCatalog);
    }

    function formatBytes(bytes) {
      if (!Number.isFinite(bytes) || bytes <= 0) return `${number(0)}\u00a0${t('units.B')}`;
      const units = ['B', 'KB', 'MB', 'GB', 'TB'];
      let value = bytes;
      let idx = 0;
      while (value >= 1024 && idx < units.length - 1) {
        value /= 1024;
        idx++;
      }
      const digits = value >= 10 || idx === 0 ? 0 : 1;
      return `${number(value, { minimumFractionDigits: digits, maximumFractionDigits: digits })}\u00a0${t('units.' + units[idx])}`;
    }

    function formatExpiry(value) {
      const expires = new Date(value);
      if (!Number.isFinite(expires.getTime()) || expires.getUTCFullYear() <= 1) return t('downloads.noExpiry');
      const diffMs = expires.getTime() - Date.now();
      const absMs = Math.abs(diffMs);
      const units = [
        // Long countdowns use approximate years and months.
        { label: 'year', ms: 365 * 24 * 60 * 60 * 1000 },
        { label: 'month', ms: 30 * 24 * 60 * 60 * 1000 },
        { label: 'day', ms: 24 * 60 * 60 * 1000 },
        { label: 'hour', ms: 60 * 60 * 1000 },
        { label: 'minute', ms: 60 * 1000 },
      ];
      const unit = units.find(u => absMs >= u.ms) || units[units.length - 1];
      const valueRounded = Math.max(1, Math.round(absMs / unit.ms));
      const part = (label, count) => t('downloads.duration.' + label, { count }).replaceAll(' ', '\u00a0');
      let duration = part(unit.label, valueRounded);
      if (unit.label === 'day' && valueRounded >= 7) {
        duration = part(diffMs < 0 ? 'weekPast' : 'week', Math.floor(valueRounded / 7));
        const days = valueRounded % 7;
        if (days) {
          duration = t('downloads.duration.weeksAndDays', { weeks: duration, days: part('day', days) });
        }
      }
      return t(diffMs < 0 ? 'downloads.expiredAgo' : 'downloads.remaining', { duration });
    }

    function formatDownloadSize(download) {
      const diskSize = Number.isFinite(download.diskSize) ? download.diskSize : 0;
      return `${formatBytes(diskSize)} / ${formatBytes(download.size)}`;
    }

    function formatDownloadState(download) {
      if (download.status === 'ready') return formatExpiry(download.expiresAt);
      if (download.status === 'preparing') return t('downloads.preparing');
      if (download.status === 'failed') return t('progress.failed');
      if (download.status === 'expired') return t('downloads.expired');
      return t('downloads.selectVideo');
    }

    function preparationStage(progress) {
      if (progress.stage === 'playable') return t('progress.playable');
      if (progress.stage === 'preparing') return t('progress.preparing');
      return t('progress.analyzing');
    }

    function preparationDetails(progress) {
      const parts = [t('progress.downloaded', { downloaded: formatBytes(progress.downloadedBytes), total: formatBytes(progress.totalBytes) })];
      if (progress.totalBytes > 0) {
        const percent = Math.min(100, Math.floor(100 * progress.downloadedBytes / progress.totalBytes));
        parts[0] += ` (${number(percent / 100, { style: 'percent' })})`;
      }
      if (progress.totalBytes > 0 && progress.downloadedBytes < progress.totalBytes) {
        parts.push(Number.isFinite(progress.downloadSpeed) ? t('progress.speed', { speed: formatBytes(progress.downloadSpeed) }) : t('progress.measuring'));
        parts.push(t('progress.peers', { count: progress.peers }));
      }
      return parts.join(' · ');
    }

    function formatPlaybackTime(time) {
      const seconds = Math.floor(time);
      const hours = Math.floor(seconds / 3600);
      const minutes = Math.floor(seconds / 60) % 60;
      return `${hours ? `${hours}:${String(minutes).padStart(2, '0')}` : minutes}:${String(seconds % 60).padStart(2, '0')}`;
    }

    function preparedVideo(progress) {
      if (Math.floor(progress.preparedSeconds) <= 0) return t('progress.waitingSegment');
      return t('progress.prepared', { time: formatPlaybackTime(progress.preparedSeconds) });
    }

    function readPlaybackPosition(download) {
      try {
        const position = JSON.parse(localStorage.getItem(`cinemator-playback:${download.id}`));
        if (position && Number.isFinite(position.time) && position.time >= 1 &&
            Number.isInteger(position.fileIndex) && download.files?.some(file => file.index === position.fileIndex)) {
          return position;
        }
      } catch {}
      return null;
    }

    function rememberPlayback(video, magnet, fileIndex) {
      let lastSavedAt = -Infinity;
      function savePosition() {
        if (video !== $('video')) return;
        const download = downloadCatalog.find(download => download.magnet === magnet);
        if (!download) return;
        const time = video.currentTime;
        if (!video.ended && (!Number.isFinite(time) || time < 1)) return;
        lastSavedAt = performance.now();
        try {
          const key = `cinemator-playback:${download.id}`;
          if (video.ended) localStorage.removeItem(key);
          else {
            const position = { fileIndex, time };
            // An unfinished EVENT playlist reports only its prepared duration.
            if (download.status === 'ready' && download.selectedFileIndex === fileIndex &&
                Number.isFinite(video.duration) && video.duration > 0) position.duration = video.duration;
            localStorage.setItem(key, JSON.stringify(position));
          }
        } catch {
          return;
        }
        renderDownloads(downloadCatalog);
      }
      savePlaybackPosition = savePosition;
      video.addEventListener('timeupdate', () => {
        if (!video.paused && performance.now() - lastSavedAt >= 5000) savePosition();
      });
      for (const event of ['pause', 'seeked', 'ended']) video.addEventListener(event, savePosition);
    }

    function updateDownloadSamples(downloads) {
      const active = new Set();
      downloads.forEach(download => {
        const progress = download.progress;
        if (!progress) return;
        active.add(download.id);
        const previous = downloadSamples.get(download.id);
        const elapsed = previous ? (Date.parse(progress.sampledAt) - Date.parse(previous.sampledAt)) / 1000 : 0;
        if (previous && previous.fileIndex === progress.fileIndex && progress.receivedBytes >= previous.receivedBytes) {
          if (elapsed >= 0.5 && elapsed <= 10) {
            progress.downloadSpeed = (progress.receivedBytes - previous.receivedBytes) / elapsed;
          } else if (elapsed > 0 && elapsed < 0.5) {
            progress.downloadSpeed = previous.downloadSpeed;
            return;
          }
        }
        downloadSamples.set(download.id, progress);
      });
      for (const id of downloadSamples.keys()) {
        if (!active.has(id)) downloadSamples.delete(id);
      }
    }

    function renderPreparationProgress() {
      const download = downloadCatalog.find(item => item.id === activeDownloadID);
      const progress = download?.progress;
      const matches = progress && progress.fileIndex === selectedVideoIndex;
      const selected = download?.selectedFileIndex === selectedVideoIndex;
      let stage = t('progress.selectedVideo');
      let details = '';
      if (matches) {
        stage = preparationStage(progress);
        details = preparationDetails(progress);
      } else if (selected && download.status === 'failed') {
        stage = t('progress.failed');
        details = t('errors.preparation');
      } else if (selected && download.status === 'ready') {
        stage = t('progress.ready');
      }
      if (flowRequestController && (matches && progress.stage === 'playable' || selected && download.status === 'ready')) {
        stage = t('progress.selectedStream');
      }
      if (progressUnavailable) {
        stage = t('progress.reconnectingServer');
        details = t('progress.unavailable');
      }
      $('preparationStage').textContent = stage;
      const bar = $('preparationProgress');
      bar.hidden = !matches || progressUnavailable || progress.totalBytes <= 0;
      if (!bar.hidden) bar.value = Math.min(1, progress.downloadedBytes / progress.totalBytes);
      $('preparationDetails').textContent = details;
      renderErrorDetails($('preparationDetails').parentElement,
        selected && download.status === 'failed' ? download.preparationError : '');
      $('preparationVideo').textContent = matches && !progressUnavailable ? preparedVideo(progress) : '';
    }

    function closeDownloadMenus(restoreFocus = false) {
      const menu = document.querySelector('.download-menu.open');
      if (!menu) return;
      downloadMenuResizeObserver.disconnect();
      menu.classList.remove('open');
      menu.querySelector('.download-options').hidden = true;
      const toggle = menu.querySelector('.download-more');
      toggle.setAttribute('aria-expanded', 'false');
      openDownloadMenuID = null;
      if (restoreFocus) toggle.focus({ preventScroll: true });
    }

    function positionDownloadMenu(menu) {
      const popup = menu.querySelector('.download-options');
      const anchor = menu.querySelector('.download-more').getBoundingClientRect();
      const box = popup.getBoundingClientRect();
      popup.style.setProperty('--menu-left', `${Math.max(8, anchor.right - box.width)}px`);
      popup.style.setProperty('--menu-top', `${anchor.bottom + box.height + 8 <= innerHeight ? anchor.bottom + 4 : anchor.top - box.height - 4}px`);
      popup.style.setProperty('--menu-height', `${box.height}px`);
    }

    function openDownloadMenu(menu) {
      menu.querySelector('.download-options').hidden = false;
      menu.classList.add('open');
      const toggle = menu.querySelector('.download-more');
      toggle.setAttribute('aria-expanded', 'true');
      openDownloadMenuID = toggle.dataset.id;
      positionDownloadMenu(menu);
      downloadMenuResizeObserver.observe(menu.querySelector('.download-options'));
    }

    function toggleDownloadMenu(toggle) {
      closeVideoMenu();
      closePlayerMenu();
      const menu = toggle.closest('.download-menu');
      const willOpen = !menu.classList.contains('open');
      closeDownloadMenus();
      if (willOpen) {
        openDownloadMenu(menu);
        [...menu.querySelectorAll('[role="menuitem"]')].find(item => item.getClientRects().length)?.focus({ preventScroll: true });
      }
    }

    function renderThumbnail(image, download, file) {
      const url = file && download.magnet && download.status === 'ready' ? `/api/hls/${encodeURIComponent(download.id)}_${file.index}_a-1_s-1/thumbnail-v3.jpg` : '';
      if (!url) {
        image.removeAttribute('src');
        delete image.dataset.url;
        image.classList.remove('loaded');
        return;
      }
      const changed = image.dataset.url !== url;
      if (!changed && (!image.dataset.retryAt || Date.now() < Number(image.dataset.retryAt))) return;
      image.dataset.url = url;
      delete image.dataset.retryAt;
      image.classList.remove('loaded');
      image.onload = () => image.classList.add('loaded');
      image.onerror = () => {
        image.classList.remove('loaded');
        image.dataset.retryAt = String(Date.now() + 30000);
      };
      image.src = url + (changed ? '' : `?retry=${Math.floor(Date.now() / 30000)}`);
    }

    function renderDownloads(downloads) {
      const list = $('downloadsList');
      $('downloadsCount').textContent = number(downloads.length);
      const focused = list.contains(document.activeElement) ? document.activeElement : null;
      const restoreMenuID = openDownloadMenuID;
      openDownloadMenuID = null;
      downloadMenuResizeObserver.disconnect();
      if (!downloads.length) {
        const empty = document.createElement('div');
        empty.className = 'downloads-empty';
        empty.textContent = t('downloads.empty');
        list.replaceChildren(empty);
        return;
      }

      const template = $('downloadRowTemplate');
      const rows = new Map(Array.from(list.querySelectorAll('.download-row'), row => [row.dataset.id, row]));
      list.querySelector('.downloads-empty')?.remove();
      downloads.forEach((download, index) => {
        const row = rows.get(download.id) || template.content.firstElementChild.cloneNode(true);
        rows.delete(download.id);
        row.dataset.id = download.id;
        row.dataset.status = download.status;
        row.classList.toggle('active', download.id === activeDownloadID);
        row.querySelectorAll('button[data-action]').forEach(button => {
          button.dataset.id = download.id;
        });
        if (download.id === activeDownloadID) {
          row.querySelector('.download-open').setAttribute('aria-current', 'true');
        } else {
          row.querySelector('.download-open').removeAttribute('aria-current');
        }
        const files = download.files || [];
        const position = readPlaybackPosition(download);
        const selectedIndex = download.id === activeDownloadID && !$('playerWorkspace').hidden ?
          selectedVideoIndex : position?.fileIndex ?? download.selectedFileIndex;
        const file = files.find(file => file.index === selectedIndex) || files[0];
        row.dataset.fileIndex = file?.index ?? '';
        const title = download.title ? (files.length === 1 ? fileStem(download.title) : download.title) : t('downloads.torrent', { id: download.id.slice(0, 8) });
        row.querySelector('.download-title').textContent = title;
        const expiry = row.querySelector('.download-expiry');
        const expiresAt = new Date(download.expiresAt);
        expiry.hidden = download.status !== 'ready' && (!Number.isFinite(expiresAt.getTime()) || expiresAt.getUTCFullYear() <= 1);
        expiry.textContent = formatExpiry(download.expiresAt);
        expiry.id = `download-expiry-${download.id}`;
        if (expiry.hidden) row.querySelector('.download-open').removeAttribute('aria-describedby');
        else row.querySelector('.download-open').setAttribute('aria-describedby', expiry.id);
        const videosButton = row.querySelector('.download-videos');
        videosButton.hidden = files.length <= 1;
        setText(videosButton, 'downloads.videos', { count: files.length });
        const saved = position?.fileIndex === file?.index ? position : null;
        row.querySelector('.download-open').setAttribute('aria-label', [
          saved ? t('downloads.resume', { time: formatPlaybackTime(saved.time) }) : t('player.play'), title, files.length > 1 ? file?.name : '',
        ].filter(Boolean).join(' · '));
        row.querySelector('[data-action="restart"]').hidden = !download.magnet || !file;
        const duration = saved?.duration;
        const durationLabel = row.querySelector('.download-duration');
        durationLabel.hidden = !Number.isFinite(duration) || duration <= 0;
        if (!durationLabel.hidden) durationLabel.textContent = formatPlaybackTime(duration);
        const watched = row.querySelector('.download-watched');
        watched.hidden = durationLabel.hidden;
        if (!watched.hidden) watched.style.setProperty('--watched-percent', `${Math.min(100, 100 * saved.time / duration)}%`);
        renderThumbnail(row.querySelector('.download-preview'), download, file);
        row.querySelector('.download-size').textContent = formatDownloadSize(download);
        row.querySelector('.download-meta-text').textContent = formatDownloadState(download);
        row.querySelector('.download-meta-text').classList.remove('has-progress');
        renderErrorDetails(row, download.preparationError);
        row.querySelector('.download-progress').hidden = true;
        const progressAtPlayer = download.id === activeDownloadID && !$('warnMsg').hidden &&
          download.progress?.fileIndex === selectedVideoIndex;
        if (progressAtPlayer) {
          row.querySelector('.download-meta-text').textContent = progressUnavailable ? t('progress.reconnecting') :
            download.progress.stage === 'playable' && !flowRequestController ? t('downloads.ready') : t('downloads.preparing');
        } else if (download.progress) {
          row.querySelector('.download-size').textContent = '';
          const meta = row.querySelector('.download-meta-text');
          meta.classList.add('has-progress');
          meta.textContent = progressUnavailable ? t('progress.reconnectingDetails') :
            `${preparationStage(download.progress)} · ${preparationDetails(download.progress)} · ${preparedVideo(download.progress)}`;
          const bar = row.querySelector('.download-progress');
          bar.hidden = progressUnavailable || download.progress.totalBytes <= 0;
          if (!bar.hidden) bar.value = Math.min(1, download.progress.downloadedBytes / download.progress.totalBytes);
        }

        const status = row.querySelector('.download-card-status');
        status.hidden = download.status === 'ready' || download.status === 'awaiting_selection' && files.length > 0;
        status.textContent = progressAtPlayer ? t('downloads.preparing') : download.progress ?
          `${preparationStage(download.progress)} · ${preparedVideo(download.progress)}` : formatDownloadState(download);
        const menu = row.querySelector('.download-menu');
        menu.querySelector('.download-extend-group').hidden = download.status !== 'ready';
        menu.querySelectorAll('[role="menuitem"]').forEach(item => { item.tabIndex = -1; });
        const toggle = menu.querySelector('.download-more');
        toggle.setAttribute('aria-label', `${t('downloads.actions')} · ${title}`);
        toggle.removeAttribute('data-i18n-aria-label');
        if (download.id === restoreMenuID) {
          openDownloadMenu(menu);
        } else {
          menu.classList.remove('open');
          menu.querySelector('.download-options').hidden = true;
          toggle.setAttribute('aria-expanded', 'false');
        }
        if (list.children[index] !== row) list.insertBefore(row, list.children[index] || null);
      });
      for (const row of rows.values()) row.remove();
      const openMenu = list.querySelector('.download-menu.open');
      if (openMenu) positionDownloadMenu(openMenu);
      if (focused?.isConnected && document.activeElement !== focused) focused.focus({ preventScroll: true });
      if (videoMenuState) renderVideoMenu();
    }

    async function loadDownloads({ quiet = false, suppressError = false, progressOnly = false } = {}) {
      if (downloadsLoading) {
        downloadsRefreshQueued = true;
        return;
      }
      downloadsLoading = true;
      if (!quiet) showMsg('downloadsMsg', 'downloads.loading', false, true);
      try {
        const res = await apiFetch('/api/downloads' + (progressOnly ? '?progress=1' : ''));
        if (!res.ok) throw await responseError(res, 'errors.downloads');
        const downloads = await res.json();
        if (progressOnly) {
          const diskSizes = new Map(downloadCatalog.map(download => [download.id, download.diskSize]));
          downloads.forEach(download => { download.diskSize = diskSizes.get(download.id); });
        } else {
          lastDownloadCatalogRefreshAt = Date.now();
        }
        downloadCatalog = downloads;
        updateDownloadSamples(downloadCatalog);
        progressUnavailable = false;
        if (!downloadCatalog.some(download => download.id === activeDownloadID)) {
          const currentMagnet = activeMagnet;
          activeDownloadID = downloadCatalog.find(download => download.magnet === currentMagnet)?.id || null;
        }
        renderDownloads(downloadCatalog);
        renderPreparationProgress();
        if (!quiet) showMsg('downloadsMsg', '');
      } catch (e) {
        progressUnavailable = true;
        renderDownloads(downloadCatalog);
        renderPreparationProgress();
        if (!suppressError) {
          showError('downloadsMsg', e, 'errors.downloads');
        }
      } finally {
        downloadsLoading = false;
        scheduleDownloadRefresh();
        if (downloadsRefreshQueued) {
          downloadsRefreshQueued = false;
          loadDownloads({ quiet: true, suppressError: true });
        }
      }
    }

    function findDownload(id) {
      return downloadCatalog.find(download => download.id === id);
    }

    function setActiveDownload(id) {
      activeDownloadID = id;
      document.querySelectorAll('.download-row[data-id]').forEach(row => {
        const isActive = row.dataset.id === id;
        row.classList.toggle('active', isActive);
        const openBtn = row.querySelector('.download-open');
        if (!openBtn) return;
        if (isActive) {
          openBtn.setAttribute('aria-current', 'true');
        } else {
          openBtn.removeAttribute('aria-current');
        }
      });
    }

    function openDownload(download, position = null, fileIndex = download?.selectedFileIndex) {
      if (!download || !download.magnet || !download.files || download.files.length === 0) {
        showMsg('downloadsMsg', 'downloads.incomplete', true);
        return;
      }
      cancelFlowRequest();
      destroyVideoAndHls();
      pendingResume = position ? { ...position, magnet: download.magnet } : null;
      if (!position) {
        try { localStorage.removeItem(`cinemator-playback:${download.id}`); } catch {}
      }
      setActiveDownload(download.id);
      activeMagnet = download.magnet;
      setSourceOpen(false);
      setFileList(download.files);
      fileIndex = position?.fileIndex ?? fileIndex;
      if (videoFiles.some(file => file.index === fileIndex)) selectedVideoIndex = fileIndex;
      $('step-files').style.display = '';
      resetTracks();
      $('player-block').style.display = 'none';
      showMsg('magnetMsg', '');
      showMsg('fileMsg', '');
      showMsg('trackMsg', '');
      showMsg('downloadsMsg', '');
      removeWarning();
      updateSelectedFile();
      launchPlayback();
      window.scrollTo({ top: 0 });
      $('closePlayer').focus({ preventScroll: true });
    }

    function closePlayer({ restoreFocus = true } = {}) {
      const id = activeDownloadID;
      cancelFlowRequest();
      destroyVideoAndHls();
      pendingResume = null;
      activeMagnet = '';
      setActiveDownload(null);
      resetTracks();
      updateMediaSession();
      removeWarning();
      $('playerWorkspace').hidden = true;
      $('player-block').style.display = 'none';
      $('step-files').style.display = 'none';
      showMsg('playerMsg', '');
      showMsg('fileMsg', '');
      showMsg('trackMsg', '');
      if (restoreFocus) {
        const row = Array.from($('downloadsList').children).find(row => row.dataset.id === id);
        (row?.querySelector('.download-open') || $('addDownloadBtn')).focus();
      }
    }
    $('closePlayer').onclick = () => closePlayer();

    async function extendDownload(id, days) {
      showMsg('downloadsMsg', 'downloads.extending', false, true);
      try {
        const res = await apiFetch(`/api/downloads/${encodeURIComponent(id)}/extend`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ days }),
        });
        if (!res.ok) throw await responseError(res, 'errors.extend');
        await loadDownloads({ quiet: true });
        showMsg('downloadsMsg', 'downloads.extended', false, false, { count: days });
      } catch (e) {
        showError('downloadsMsg', e, 'errors.extend');
      }
    }

    async function deleteDownload(id) {
      const download = findDownload(id);
      if (!download || !window.confirm(t('downloads.confirmDelete', { title: download.title || id }))) return;
      const deletingCurrentDownload = activeDownloadID === id;
      showMsg('downloadsMsg', 'downloads.deleting', false, true);
      try {
        const res = await apiFetch(`/api/downloads/${encodeURIComponent(id)}`, { method: 'DELETE' });
        if (!res.ok) throw await responseError(res, 'errors.delete');
        if (deletingCurrentDownload && activeDownloadID === id) {
          closePlayer({ restoreFocus: false });
          $('addDownloadBtn').focus();
        }
        await loadDownloads({ quiet: true });
        if (!document.activeElement || document.activeElement === document.body) $('addDownloadBtn').focus();
        showMsg('downloadsMsg', 'downloads.deleted');
      } catch (e) {
        showError('downloadsMsg', e, 'errors.delete');
      }
    }

    function scheduleDownloadRefresh(delay) {
      clearTimeout(downloadPollingTimer);
      downloadPollingTimer = null;
      if (document.visibilityState === 'hidden') return;
      const preparing = downloadCatalog.some(download => download.status === 'preparing');
      if (delay === undefined) {
        if (preparing || progressUnavailable) delay = 2000;
        else if (!downloadEvents || downloadEvents.readyState !== EventSource.OPEN) delay = downloadFallbackPollingMs;
        else return;
      }
      downloadPollingTimer = setTimeout(() => {
        downloadPollingTimer = null;
        loadDownloads({ quiet: true, suppressError: true,
          progressOnly: Date.now() - lastDownloadCatalogRefreshAt < downloadFallbackPollingMs });
      }, delay);
    }

    function startDownloadEvents() {
      if (!window.EventSource) {
        scheduleDownloadRefresh(0);
        return;
      }

      const events = new EventSource('/api/downloads/events');
      downloadEvents = events;
      events.addEventListener('open', () => {
        if (downloadEvents === events) {
          if (!downloadPollingTimer || !downloadCatalog.some(download => download.status === 'preparing')) {
            scheduleDownloadRefresh();
          }
        }
      });
      events.addEventListener('changed', () => {
        loadDownloads({ quiet: true, suppressError: true });
      });
      events.addEventListener('error', () => {
        if (downloadEvents === events) {
          if (!downloadPollingTimer) scheduleDownloadRefresh(downloadFallbackInitialMs);
        }
      });
    }

    $('downloadsList').addEventListener('click', event => {
      const button = event.target.closest('button[data-action][data-id]');
      if (!button) return;
      const { action, id } = button.dataset;
      if (action === 'choose-video') return toggleVideoMenu(button, id);
      if (action === 'toggle-menu') return toggleDownloadMenu(button);
      const download = findDownload(id);
      if (action === 'open' || action === 'restart') {
        savePlaybackPosition?.();
        const fileIndex = Number(button.closest('.download-row').dataset.fileIndex);
        const position = action === 'open' ? readPlaybackPosition(download) : null;
        closeDownloadMenus();
        closeVideoMenu();
        openDownload(download, position?.fileIndex === fileIndex ? position : null, fileIndex);
      }
      if (action === 'extend') {
        closeDownloadMenus(true);
        extendDownload(id, Number(button.dataset.days));
      }
      if (action === 'delete') {
        closeDownloadMenus(true);
        deleteDownload(id);
      }
    });
    document.addEventListener('click', event => {
      if (!event.target.closest('.download-menu')) closeDownloadMenus();
    });
    document.addEventListener('focusin', event => {
      if (!event.target.closest('.download-menu')) closeDownloadMenus();
    });
    document.addEventListener('keydown', event => {
      const menu = document.querySelector('.download-menu.open');
      if (!menu) return;
      if (event.key === 'Escape') {
        event.preventDefault();
        closeDownloadMenus(true);
      } else if (['ArrowDown', 'ArrowUp', 'Home', 'End'].includes(event.key)) {
        event.preventDefault();
        const items = [...menu.querySelectorAll('[role="menuitem"]')].filter(item => item.getClientRects().length);
        const current = items.indexOf(document.activeElement);
        const next = event.key === 'Home' ? 0 : event.key === 'End' ? items.length - 1 :
          (current + (event.key === 'ArrowDown' ? 1 : -1) + items.length) % items.length;
        items[next]?.focus();
      }
    });
    for (const event of ['resize', 'scroll']) window.addEventListener(event, () => {
      const menu = document.querySelector('.download-menu.open');
      if (menu) positionDownloadMenu(menu);
    });
    window.addEventListener('focus', () => {
      loadDownloads({ quiet: true, suppressError: true });
    });
    document.addEventListener('visibilitychange', () => {
      scheduleDownloadRefresh();
      if (document.visibilityState === 'visible') {
        loadDownloads({ quiet: true, suppressError: true });
      } else {
        savePlaybackPosition?.();
      }
    });
    window.addEventListener('pagehide', () => savePlaybackPosition?.());
    window.addEventListener('beforeunload', () => {
      if (downloadEvents) downloadEvents.close();
      clearTimeout(downloadPollingTimer);
    });
    loadDownloads();
    startDownloadEvents();

    function setSourceOpen(open) {
      const panel = $('sourcePanel');
      const restoreFocus = panel.contains(document.activeElement);
      panel.hidden = !open;
      $('addDownloadBtn').setAttribute('aria-expanded', String(open));
      setText($('addDownloadLabel'), open ? 'common.close' : 'downloads.add');
      if (open) $('magnet').focus();
      else if (restoreFocus) $('addDownloadBtn').focus();
    }
    $('addDownloadBtn').onclick = () => setSourceOpen($('sourcePanel').hidden);
    $('sourcePanel').addEventListener('keydown', event => {
      if (event.key === 'Escape') {
        event.stopPropagation();
        setSourceOpen(false);
      }
    });

    $('form').onsubmit = async e => {
      e.preventDefault();
      const magnet = $('magnet').value.trim();
      if (!magnet) {
        showMsg('magnetMsg', 'player.magnetRequired', true);
        return;
      }
      filesRequestController?.abort();
      const controller = new AbortController();
      filesRequestController = controller;
      showMsg('magnetMsg', 'player.loadingFiles', false, true);
      try {
        const res = await apiFetch('/api/torrent/files?magnet=' + encodeURIComponent(magnet), { signal: controller.signal });
        if (!res.ok) throw await responseError(res, 'errors.files');
        const files = await res.json();
        if (filesRequestController !== controller) return;
        if (!files.length) throw new Error('errors.noFiles');
        await loadDownloads({ quiet: true });
        if (filesRequestController !== controller) return;
        showMsg('magnetMsg', '');
        if ($('magnet').value.trim() === magnet) {
          setSourceOpen(false);
          $('magnet').value = '';
          const download = downloadCatalog.find(item => item.magnet === magnet);
          const row = Array.from($('downloadsList').children).find(row => row.dataset.id === download?.id);
          (row?.querySelector('.download-open') || $('addDownloadBtn')).focus();
        }
      } catch (e) {
        if (e.name === 'AbortError') return;
        if (filesRequestController !== controller) return;
        if ($('sourcePanel').hidden) setSourceOpen(true);
        showError('magnetMsg', e, 'errors.files');
        return;
      } finally {
        if (filesRequestController === controller) filesRequestController = null;
      }
    };

    const videoMenuResizeObserver = new ResizeObserver(positionVideoMenu);
    function closeVideoMenu(restoreFocus = false) {
      if (!videoMenuState) return;
      const trigger = videoMenuState.trigger;
      const focusInside = $('videoMenu').contains(document.activeElement);
      videoMenuState = null;
      videoMenuResizeObserver.disconnect();
      $('videoMenu').hidden = true;
      trigger.setAttribute('aria-expanded', 'false');
      if (restoreFocus || focusInside) {
        (trigger.isConnected && !trigger.hidden ? trigger : $('addDownloadBtn')).focus({ preventScroll: true });
      }
    }
    function positionVideoMenu() {
      if (!videoMenuState) return;
      const popup = $('videoMenu');
      const anchor = videoMenuState.trigger.getBoundingClientRect();
      const box = popup.getBoundingClientRect();
      const bottom = videoMenuState.trigger === $('videoMenuButton') ?
        Math.min(anchor.top - 8, $('video').getBoundingClientRect().bottom) : anchor.top - 8;
      const above = bottom - box.height;
      const top = above >= 8 ? above : anchor.bottom + 8;
      popup.style.left = `${Math.max(8, Math.min(anchor.right - box.width, innerWidth - box.width - 8))}px`;
      popup.style.top = `${Math.max(8, Math.min(top, innerHeight - box.height - 8))}px`;
    }
    function renderVideoMenu() {
      if (!videoMenuState) return;
      const { trigger, downloadID } = videoMenuState;
      const download = downloadID === null ? null : findDownload(downloadID);
      const files = downloadID === null ? videoFiles : download?.files || [];
      if (!trigger.isConnected || trigger.hidden || files.length <= 1) {
        closeVideoMenu();
        return;
      }
      let current = downloadID === null || downloadID === activeDownloadID && !$('playerWorkspace').hidden ?
        selectedVideoIndex : readPlaybackPosition(download)?.fileIndex ?? download.selectedFileIndex;
      if (!files.some(file => file.index === current)) current = files[0].index;
      const popup = $('videoMenu');
      const items = new Map([...popup.children].map(item => [Number(item.dataset.fileIndex), item]));
      const focusedIndex = popup.contains(document.activeElement) ? Number(document.activeElement.dataset.fileIndex) : null;
      files.forEach((file, index) => {
        let item = items.get(file.index);
        items.delete(file.index);
        if (!item) {
          item = document.createElement('button');
          item.type = 'button';
          item.className = 'video-file-option';
          item.setAttribute('role', 'menuitemradio');
          item.dataset.fileIndex = String(file.index);
          item.innerHTML = '<span class="video-file-check" aria-hidden="true">✓</span><span class="video-file-name"></span>';
        }
        item.querySelector('.video-file-name').textContent = file.name;
        item.setAttribute('aria-checked', String(file.index === current));
        item.tabIndex = file.index === current ? 0 : -1;
        if (popup.children[index] !== item) popup.insertBefore(item, popup.children[index] || null);
      });
      for (const item of items.values()) item.remove();
      if (focusedIndex !== null && !popup.contains(document.activeElement)) {
        ([...popup.children].find(item => Number(item.dataset.fileIndex) === focusedIndex) ||
          popup.querySelector('[aria-checked="true"]')).focus({ preventScroll: true });
      }
      positionVideoMenu();
    }
    function toggleVideoMenu(trigger, downloadID = null) {
      const opening = videoMenuState?.trigger !== trigger;
      closeVideoMenu();
      closePlayerMenu();
      closeDownloadMenus();
      if (!opening) return;
      videoMenuState = { trigger, downloadID };
      $('videoMenu').hidden = false;
      trigger.setAttribute('aria-expanded', 'true');
      renderVideoMenu();
      if (!videoMenuState) return;
      videoMenuResizeObserver.observe($('videoMenu'));
      const selected = $('videoMenu').querySelector('[aria-checked="true"]');
      selected.focus({ preventScroll: true });
      selected.scrollIntoView({ block: 'nearest', inline: 'nearest' });
    }
    $('videoMenuButton').onclick = () => toggleVideoMenu($('videoMenuButton'));
    $('videoMenu').onclick = event => {
      const item = event.target.closest('[data-file-index]');
      if (!item || !videoMenuState) return;
      const { downloadID } = videoMenuState;
      const fileIndex = Number(item.dataset.fileIndex);
      closeVideoMenu(true);
      if (downloadID === null || downloadID === activeDownloadID && !$('playerWorkspace').hidden) {
        selectVideoFile(fileIndex);
      } else {
        openDownload(findDownload(downloadID), null, fileIndex);
      }
    };
    for (const type of ['click', 'focusin']) document.addEventListener(type, event => {
      if (videoMenuState && !videoMenuState.trigger.contains(event.target) && !$('videoMenu').contains(event.target)) {
        closeVideoMenu(type === 'click' && document.activeElement === document.body);
      }
    });
    document.addEventListener('keydown', event => {
      if (!videoMenuState) return;
      if (event.key === 'Escape' || event.key === 'Tab') {
        if (event.key === 'Escape') event.preventDefault();
        closeVideoMenu(true);
      } else if (['ArrowDown', 'ArrowUp', 'Home', 'End'].includes(event.key)) {
        event.preventDefault();
        const items = [...$('videoMenu').children];
        const current = items.indexOf(document.activeElement);
        const next = event.key === 'Home' ? 0 : event.key === 'End' ? items.length - 1 :
          (current + (event.key === 'ArrowDown' ? 1 : -1) + items.length) % items.length;
        items[next]?.focus();
      }
    });
    for (const event of ['resize', 'scroll']) window.addEventListener(event, positionVideoMenu, true);

    let openPlayerMenu = null;
    function closePlayerMenu(restoreFocus = false) {
      if (!openPlayerMenu) return;
      const button = $(openPlayerMenu + 'MenuButton');
      const popup = $(openPlayerMenu + 'Menu');
      const focusInside = popup.contains(document.activeElement);
      popup.hidden = true;
      button.setAttribute('aria-expanded', 'false');
      openPlayerMenu = null;
      if (restoreFocus || focusInside) button.focus();
    }
    for (const name of ['audio', 'subtitles']) {
      $(name + 'MenuButton').onclick = () => {
        const opening = openPlayerMenu !== name;
        closeVideoMenu();
        closeDownloadMenus();
        closePlayerMenu();
        if (!opening) return;
        openPlayerMenu = name;
        $(name + 'Menu').hidden = false;
        $(name + 'MenuButton').setAttribute('aria-expanded', 'true');
        $(name + 'Menu').querySelector('select').focus();
      };
    }
    document.addEventListener('click', event => {
      if (!event.target.closest('.player-setting')) closePlayerMenu(document.activeElement === document.body);
    });
    document.addEventListener('keydown', event => {
      if (event.key === 'Escape' && openPlayerMenu) {
        event.preventDefault();
        closePlayerMenu(true);
      }
    });
    document.addEventListener('focusin', event => {
      if (!event.target.closest('.player-setting')) closePlayerMenu();
    });
    function updateSelectedFile() {
      const index = videoFiles.findIndex(file => file.index === selectedVideoIndex);
      const file = videoFiles[index];
      $('selectedFile').textContent = file?.name || '';
      $('selectedFile').title = file?.name || '';
      $('videoMenuButton').hidden = videoFiles.length <= 1;
      $('videoNavigation').hidden = videoFiles.length <= 1;
      $('previousVideo').disabled = index <= 0;
      $('nextVideo').disabled = index < 0 || index >= videoFiles.length - 1;
      setText($('videoMenuLabel'), 'player.videoPosition', { index: index + 1, count: videoFiles.length });
      updateMediaSession();
    }
    function navigateVideo(direction) {
      if (!wantsPlayback) return;
      const index = videoFiles.findIndex(file => file.index === selectedVideoIndex);
      const file = index >= 0 ? videoFiles[index + direction] : null;
      if (file) {
        const focused = document.activeElement;
        selectVideoFile(file.index);
        if ((focused === $('previousVideo') || focused === $('nextVideo')) && focused.disabled) $('videoMenuButton').focus();
      }
    }
    $('previousVideo').onclick = () => navigateVideo(-1);
    $('nextVideo').onclick = () => navigateVideo(1);

    function updateMediaSession() {
      const session = navigator.mediaSession;
      if (!session) return;
      const index = videoFiles.findIndex(file => file.index === selectedVideoIndex);
      const active = wantsPlayback && index >= 0;
      for (const [action, direction] of [['previoustrack', -1], ['nexttrack', 1]]) {
        const available = active && videoFiles[index + direction];
        try { session.setActionHandler(action, available ? () => navigateVideo(direction) : null); } catch {}
      }
      if (typeof MediaMetadata === 'function') {
        session.metadata = active ? new MediaMetadata({
          title: videoFiles[index].name,
          album: findDownload(activeDownloadID)?.title || '',
        }) : null;
      }
    }
    function updateSubtitleDelay() {
      $('subtitleDelayRow').style.display = Number($('subtitleSelect').value) >= 0 ? '' : 'none';
    }
    function resetTracks() {
      closePlayerMenu();
      closeVideoMenu();
      tracksReady = false;
      wantsPlayback = false;
      retryTime = 0;
      setText($('play'), 'player.play');
      $('play').hidden = false;
      setOptions($('audioSelect'), [{ value: 0, label: () => t('player.default') }]);
      setOptions($('subtitleSelect'), [{ value: -1, label: () => t('player.none') }]);
      $('audioSelect').disabled = true;
      $('subtitleSelect').disabled = true;
      updateSubtitleDelay();
    }
    function formatTrackLabel(track, type) {
      // Media containers commonly use ISO 639-2 bibliographic codes.
      const aliases = { eng: 'en', rus: 'ru', zho: 'zh', chi: 'zh', jpn: 'ja', kor: 'ko', fra: 'fr', fre: 'fr', deu: 'de', ger: 'de', spa: 'es', ita: 'it', por: 'pt', ukr: 'uk' };
      let language = track.language;
      if (language && language !== 'und') {
        try { language = new Intl.DisplayNames([document.documentElement.lang], { type: 'language' }).of(aliases[language.toLowerCase()] || language); } catch {}
      } else language = '';
      let label = language || t(type === 'audio' ? 'player.track' : 'player.subtitle', { index: track.index + 1 });
      if (track.title) label += ` — ${track.title}`;
      if (track.codec) label += ` (${track.codec})`;
      return label;
    }

    async function loadTracks() {
      const magnet = activeMagnet;
      const idx = selectedVideoIndex;
      if (!magnet || idx === null) return;
      const request = beginFlowRequest();
      showMsg('fileMsg', '');
      showWarning();
      try {
        const prepare = await apiFetch(`/api/hls/prepare?magnet=${encodeURIComponent(magnet)}&file=${idx}`, { method: 'POST', signal: request.signal });
        if (isStale(request.id)) return;
        if (!prepare.ok) throw await responseError(prepare, 'errors.startPreparation');
        loadDownloads({ quiet: true });
        const res = await apiFetch(`/api/media/info?magnet=${encodeURIComponent(magnet)}&file=${idx}`, { signal: request.signal });
        if (!res.ok) throw await responseError(res, 'errors.mediaInfo');
        const info = await res.json();
        if (isStale(request.id)) return;
        setOptions($('audioSelect'), info.audioTracks?.length ? info.audioTracks.map(track => ({ value: track.index, label: () => formatTrackLabel(track, 'audio') })) : [{ value: 0, label: () => t('player.default') }]);
        const preferred = preferredAudioIndex(activeDownloadID);
        if (info.audioTracks?.some(track => track.index === preferred)) $('audioSelect').value = String(preferred);
        setOptions($('subtitleSelect'), [{ value: -1, label: () => t('player.none') }, ...(info.subtitles || []).map(track => ({ value: track.index, label: () => formatTrackLabel(track, 'subtitle') }))]);
        tracksReady = true;
        $('audioSelect').disabled = false;
        $('subtitleSelect').disabled = false;
        updateSubtitleDelay();
        showMsg('fileMsg', '');
        removeWarning();
        if (wantsPlayback) startPlayback();
      } catch (error) {
        if (error.name === 'AbortError' || isStale(request.id)) return;
        removeWarning();
        showError('fileMsg', error, 'errors.tracks');
        setText($('play'), 'player.retry');
        $('play').hidden = false;
      } finally { finishFlowRequest(request); }
    }

    function launchPlayback() {
      wantsPlayback = true;
      updateMediaSession();
      $('playerWorkspace').hidden = false;
      $('step-files').style.display = '';
      closePlayerMenu(true);
      $('player-block').style.display = '';
      document.body.classList.add('has-player');
      $('play').hidden = true;
      if (tracksReady) startPlayback();
      else if (!flowRequestController) loadTracks();
    }
    function selectVideoFile(fileIndex) {
      if (fileIndex === selectedVideoIndex) return;
      selectedVideoIndex = fileIndex;
      const restart = wantsPlayback;
      cancelFlowRequest();
      pendingResume = null;
      destroyVideoAndHls({ resetLayout: !restart });
      try { localStorage.removeItem(`cinemator-playback:${activeDownloadID}`); } catch {}
      resetTracks();
      updateSelectedFile();
      showMsg('fileMsg', '');
      showMsg('trackMsg', '');
      showMsg('playerMsg', '');
      removeWarning();
      if (restart) launchPlayback();
    }

    async function startPlayback(resumeTime) {
      const magnet = activeMagnet;
      const idx = selectedVideoIndex;
      const audio = $('audioSelect').value || '0';
      const subtitle = $('subtitleSelect').value || '-1';
      const subtitleSelected = parseInt(subtitle, 10) >= 0;
      if (!magnet || idx === null) {
        showMsg('trackMsg', 'player.selectFile', true);
        return;
      }
      resumeTime ??= pendingResume?.magnet === magnet && pendingResume.fileIndex === Number(idx) ? pendingResume.time : retryTime;
      retryTime = resumeTime;
      const request = beginFlowRequest();
      const requestId = request.id;
      const wasPlaying = $('player-block').style.display !== 'none' || document.body.classList.contains('has-player');
      destroyVideoAndHls({ resetLayout: !wasPlaying });
      $('player-block').style.display = '';
      document.body.classList.add('has-player');
      $('play').hidden = true;
      removeWarning();
      showWarning();
      showMsg('trackMsg', '');
      showMsg('playerMsg', '');
      lastStreamActivityAt = Date.now();

      if (subtitleSelected) {
        clearSubtitleWait();
        subtitleWaitTimer = setTimeout(() => {
          setWarningExtra('player.waitingSubtitles');
        }, 8000);
      }
      try {
        const resp = await apiFetch(`/api/hls/prepare?magnet=${encodeURIComponent(magnet)}&file=${idx}&audio=${audio}&subtitle=${subtitle}`, {
          redirect: 'follow',
          signal: request.signal,
        });
        if (!resp.ok) throw await responseError(resp, 'errors.stream');
        if (isStale(requestId)) return;
        const m3u8 = resp.url.replace(window.location.origin, '') + '?t=' + Date.now();
        $('player-block').style.display = '';
        document.body.classList.add('has-player');
        rememberPlayback($('video'), magnet, Number(idx));
        playHls(m3u8, subtitleSelected, resumeTime);
        pendingResume = null;
        loadDownloads({ quiet: true });
      } catch (e) {
        if (e.name === 'AbortError') return;
        if (isStale(requestId)) return;
        removeWarning();
        showError('trackMsg', e, 'errors.stream');
        setText($('play'), 'player.retry');
        $('play').hidden = false;
        return;
      } finally {
        finishFlowRequest(request);
      }
    }

    $('play').onclick = launchPlayback;
    ['audioSelect', 'subtitleSelect'].forEach(id => {
      $(id).addEventListener('change', () => {
        if (id === 'audioSelect' && activeDownloadID && tracksReady) {
          const index = Number($('audioSelect').value);
          if (Number.isSafeInteger(index) && index >= 0) {
            audioPreferences.set(activeDownloadID, index);
            try { localStorage.setItem(`cinemator-audio:${activeDownloadID}`, String(index)); } catch {}
          }
        }
        updateSubtitleDelay();
        if (wantsPlayback && tracksReady) startPlayback($('video').readyState > 0 ? $('video').currentTime : retryTime);
      });
    });

    function applySubtitleDelay(video, delaySec) {
      if (!video || !video.textTracks) return;
      const track = Array.from(video.textTracks).find(t => t.kind === 'subtitles' || t.kind === 'captions');
      if (!track || !track.cues) return;
      for (let i = 0; i < track.cues.length; i++) {
        const cue = track.cues[i];
        if (cue.__origStart === undefined) {
          cue.__origStart = cue.startTime;
          cue.__origEnd = cue.endTime;
        }
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

    function showPlaybackError(details) {
      retryTime = $('video').readyState > 0 ? $('video').currentTime : retryTime;
      setText($('play'), 'player.retry');
      $('play').hidden = false;
      removeWarning();
      showError('playerMsg', { detail: details }, 'player.playbackError');
    }

    function playHls(src, enableSubtitles, resumeTime = 0) {
      const video = $('video');
      video.style.opacity = 0;
      setTimeout(() => { video.style.opacity = 1; }, 120);
      lastStreamActivityAt = Date.now();
      const current = () => video === $('video');
      video.addEventListener('error', () => { if (current()) showPlaybackError('Native media error'); });

      const reapplySubtitleDelay = () => applySubtitleDelay(video, subtitleDelay);
      if (enableSubtitles) {
        watchSubtitleUpdates(video, reapplySubtitleDelay);
      }

      let fragLoaded = false;
      function hideWarningOnce() {
        if (!fragLoaded) {
          fragLoaded = true;
          removeWarning();
        }
      }
      function markStreamActive() {
        if (!current()) return;
        $('play').hidden = true;
        lastStreamActivityAt = Date.now();
        showMsg('playerMsg', '');
        hideWarningOnce();
      }
      if (typeof Hls !== 'undefined' && Hls.isSupported()) {
        hls = new Hls({ startPosition: resumeTime });
        hls.loadSource(src);
        hls.attachMedia(video);

        hls.on(Hls.Events.MANIFEST_PARSED, () => {
          if (!current()) return;
          lastStreamActivityAt = Date.now();
          if (resumeTime > 0) {
            video.currentTime = resumeTime;
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
            if (!current()) return;
            showSubtitleTextTracks(video);
            reapplySubtitleDelay();
          });
          hls.on(Hls.Events.SUBTITLE_TRACK_SWITCH, () => {
            if (!current()) return;
            showSubtitleTextTracks(video);
            reapplySubtitleDelay();
          });
        }
        hls.on(Hls.Events.FRAG_LOADED, markStreamActive);
        hls.on(Hls.Events.ERROR, (evt, data) => {
          if (!current()) return;
          const levelEmpty = data.details === 'levelEmptyError' ||
            (Hls.ErrorDetails && data.details === Hls.ErrorDetails.LEVEL_EMPTY_ERROR);
          if (levelEmpty && (fragLoaded || Date.now() - lastStreamActivityAt < 8000)) {
            return;
          }
          if (data.fatal) {
            showPlaybackError(data.details);
          }
        });
      } else if (video.canPlayType('application/vnd.apple.mpegurl')) {
        video.src = src;
        video.addEventListener('loadedmetadata', () => {
          if (!current()) return;
          video.currentTime = resumeTime;
        }, { once: true });
        video.addEventListener('canplay', markStreamActive, { once: true });
        video.addEventListener('playing', markStreamActive);
        video.addEventListener('loadeddata', () => {
          markStreamActive();
          if (enableSubtitles) showSubtitleTextTracks(video);
          reapplySubtitleDelay();
        }, { once: true });
      } else {
        removeWarning();
        showMsg('playerMsg', 'player.hlsUnsupported', true);
      }
    }

    function updateDelayDisplay() {
      const el = $('subDelayValue');
      if (el) setText(el, 'player.seconds', { value: number(subtitleDelay, { minimumFractionDigits: 1, maximumFractionDigits: 1 }) });
      setText($('subDelayMinus'), 'player.seconds', { value: number(-0.5) });
      setText($('subDelayPlus'), 'player.seconds', { value: number(0.5, { signDisplay: 'always' }) });
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

    document.addEventListener('cinemator:languagechange', () => {
      renderDownloads(downloadCatalog);
      renderPreparationProgress();
      updateDelayDisplay();
      for (const option of document.querySelectorAll('select option')) {
        const label = optionLabels.get(option);
        if (label) option.textContent = label();
      }
      updateSelectedFile();
    });

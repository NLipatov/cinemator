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
    let openExtendDownloadID = null;
    let activeDownloadID = null;
    let pendingResume = null;
    let savePlaybackPosition = null;
    let requestSeq = 0;
    let flowRequestController = null;
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
      oldVideo.parentNode.replaceChild(newVideo, oldVideo);
      newVideo.id = 'video';
    }
    const optionLabels = new WeakMap();
    function setOptions(select, options) {
      select.replaceChildren(...options.map(({ value, label }) => {
        const option = new Option(label(), String(value));
        optionLabels.set(option, label);
        return option;
      }));
    }
    function setFileList(files) {
      setOptions($('filelist'), files.map(f => ({
        value: f.index,
        label: () => `${f.name} (${formatBytes(f.size)})`,
      })));
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
      if (!Number.isFinite(bytes) || bytes <= 0) return `${number(0)} ${t('units.B')}`;
      const units = ['B', 'KB', 'MB', 'GB', 'TB'];
      let value = bytes;
      let idx = 0;
      while (value >= 1024 && idx < units.length - 1) {
        value /= 1024;
        idx++;
      }
      const digits = value >= 10 || idx === 0 ? 0 : 1;
      return `${number(value, { minimumFractionDigits: digits, maximumFractionDigits: digits })} ${t('units.' + units[idx])}`;
    }

    function formatExpiry(value) {
      const expires = new Date(value);
      if (!Number.isFinite(expires.getTime()) || expires.getUTCFullYear() <= 1) return t('downloads.noExpiry');
      const diffMs = expires.getTime() - Date.now();
      const absMs = Math.abs(diffMs);
      const units = [
        { label: 'day', ms: 24 * 60 * 60 * 1000 },
        { label: 'hour', ms: 60 * 60 * 1000 },
        { label: 'minute', ms: 60 * 1000 },
      ];
      const unit = units.find(u => absMs >= u.ms) || units[units.length - 1];
      const valueRounded = Math.max(1, Math.round(absMs / unit.ms));
      return t(diffMs < 0 ? 'downloads.expiredAgo' : 'downloads.remaining', { duration: t('downloads.duration.' + unit.label, { count: valueRounded }) });
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
          else localStorage.setItem(key, JSON.stringify({ fileIndex, time }));
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
      const matches = progress && progress.fileIndex === Number($('filelist').value);
      const selected = download?.selectedFileIndex === Number($('filelist').value);
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

    function renderDownloads(downloads) {
      const list = $('downloadsList');
      const focused = list.contains(document.activeElement) ? document.activeElement : null;
      const restoreExtendID = openExtendDownloadID;
      openExtendDownloadID = null;
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
        row.classList.toggle('active', download.id === activeDownloadID);
        row.querySelectorAll('button[data-action]').forEach(button => {
          button.dataset.id = download.id;
        });
        if (download.id === activeDownloadID) {
          row.querySelector('.download-open').setAttribute('aria-current', 'true');
        } else {
          row.querySelector('.download-open').removeAttribute('aria-current');
        }
        row.querySelector('.download-title').textContent = download.title || t('downloads.torrent', { id: download.id.slice(0, 8) });
        row.querySelector('.download-subtitle').textContent = download.id;
        const resume = row.querySelector('.download-resume');
        const position = readPlaybackPosition(download);
        resume.hidden = !position;
        if (position) setText(resume, 'downloads.resume', { time: formatPlaybackTime(position.time) });
        row.querySelector('.download-meta-text').textContent = `${formatDownloadSize(download)} · ${formatDownloadState(download)}`;
        row.querySelector('.download-meta-text').classList.remove('has-progress');
        renderErrorDetails(row, download.preparationError);
        row.querySelector('.download-progress').hidden = true;
        const progressAtPlayer = download.id === activeDownloadID && !$('warnMsg').hidden &&
          download.progress?.fileIndex === Number($('filelist').value);
        if (progressAtPlayer) {
          row.querySelector('.download-meta-text').textContent = progressUnavailable ? t('progress.reconnecting') :
            download.progress.stage === 'playable' && !flowRequestController ? t('downloads.ready') : t('downloads.preparing');
        } else if (download.progress) {
          const meta = row.querySelector('.download-meta-text');
          meta.classList.add('has-progress');
          meta.textContent = progressUnavailable ? t('progress.reconnectingDetails') :
            `${preparationStage(download.progress)} · ${preparationDetails(download.progress)} · ${preparedVideo(download.progress)}`;
          const bar = row.querySelector('.download-progress');
          bar.hidden = progressUnavailable || download.progress.totalBytes <= 0;
          if (!bar.hidden) bar.value = Math.min(1, download.progress.downloadedBytes / download.progress.totalBytes);
        }

        const extendMenu = row.querySelector('.download-menu');
        extendMenu.hidden = download.status !== 'ready';
        if (download.status === 'ready' && download.id === restoreExtendID) {
          openExtendMenu(extendMenu);
        } else {
          extendMenu.classList.remove('open');
          extendMenu.querySelector('.download-extend-menu').hidden = true;
          extendMenu.querySelector('[data-action="toggle-extend"]').setAttribute('aria-expanded', 'false');
        }
        if (list.children[index] !== row) list.insertBefore(row, list.children[index] || null);
      });
      for (const row of rows.values()) row.remove();
      if (focused?.isConnected && document.activeElement !== focused) focused.focus({ preventScroll: true });
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
          const currentMagnet = $('magnet').value.trim();
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

    function openDownload(download, position = null) {
      if (!download || !download.magnet || !download.files || download.files.length === 0) {
        showMsg('downloadsMsg', 'downloads.incomplete', true);
        return;
      }
      cancelFlowRequest();
      destroyVideoAndHls();
      pendingResume = position ? { ...position, magnet: download.magnet } : null;
      setActiveDownload(download.id);
      $('magnet').value = download.magnet;
      setFileList(download.files);
      const fileIndex = position?.fileIndex ?? download.selectedFileIndex;
      if (Number.isInteger(fileIndex)) {
        $('filelist').value = String(fileIndex);
      }
      $('step-files').style.display = '';
      $('step-tracks').style.display = 'none';
      $('player-block').style.display = 'none';
      showMsg('magnetMsg', '');
      showMsg('fileMsg', '');
      showMsg('trackMsg', '');
      showMsg('downloadsMsg', '');
      removeWarning();
      if (position) $('selectTracks').click();
    }

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
          cancelFlowRequest();
          destroyVideoAndHls();
          setActiveDownload(null);
          $('magnet').value = '';
          $('filelist').textContent = '';
          $('step-files').style.display = 'none';
          $('step-tracks').style.display = 'none';
          $('player-block').style.display = 'none';
          removeWarning();
        }
        await loadDownloads({ quiet: true });
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

    $('downloadsList').addEventListener('click', e => {
      const extendToggle = e.target.closest('button[data-action="toggle-extend"][data-id]');
      if (extendToggle) {
        toggleExtendMenu(extendToggle);
        return;
      }
      const btn = e.target.closest('button[data-action][data-id]');
      if (btn) {
        if (btn.dataset.action === 'open') openDownload(findDownload(btn.dataset.id));
        if (btn.dataset.action === 'resume') {
          const download = findDownload(btn.dataset.id);
          if (download) openDownload(download, readPlaybackPosition(download));
        }
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

    $('form').onsubmit = async e => {
      e.preventDefault();
      const magnet = $('magnet').value.trim();
      if (!magnet) {
        showMsg('magnetMsg', 'player.magnetRequired', true);
        return;
      }
      pendingResume = null;
      setActiveDownload(downloadCatalog.find(download => download.magnet === magnet)?.id || null);
      const request = beginFlowRequest();
      const requestId = request.id;
      destroyVideoAndHls();
      showMsg('magnetMsg', 'player.loadingFiles', false, true);
      $('filelist').textContent = '';
      $('step-files').style.display = 'none';
      $('step-tracks').style.display = 'none';
      $('player-block').style.display = 'none';
      removeWarning();
      try {
        const res = await apiFetch('/api/torrent/files?magnet=' + encodeURIComponent(magnet), { signal: request.signal });
        if (!res.ok) throw await responseError(res, 'errors.files');
        const files = await res.json();
        if (isStale(requestId)) return;
        if (!files.length) throw new Error('errors.noFiles');
        setFileList(files);
        $('step-files').style.display = '';
        showMsg('magnetMsg', '');
        loadDownloads({ quiet: true });
      } catch (e) {
        if (e.name === 'AbortError') return;
        if (isStale(requestId)) return;
        showError('magnetMsg', e, 'errors.files');
        return;
      } finally {
        finishFlowRequest(request);
      }
    };

    function formatTrackLabel(track, type) {
      let label = t(type === 'audio' ? 'player.track' : 'player.subtitle', { index: track.index + 1 });
      if (track.language) label += ` [${track.language}]`;
      if (track.title) label += ` - ${track.title}`;
      if (track.codec) label += ` (${track.codec})`;
      return label;
    }

    $('selectTracks').onclick = async () => {
      const magnet = $('magnet').value.trim();
      const idx = $('filelist').value;
      if (!magnet || idx === undefined || idx === '') {
        showMsg('fileMsg', 'player.selectFile', true);
        return;
      }
      const request = beginFlowRequest();
      const requestId = request.id;
      showMsg('fileMsg', 'player.loadingTracks', false, true);
      $('step-tracks').style.display = 'none';
      showWarning();
      try {
        const prepare = await apiFetch(`/api/hls/prepare?magnet=${encodeURIComponent(magnet)}&file=${idx}`, {
          method: 'POST',
          signal: request.signal,
        });
        if (!prepare.ok) throw await responseError(prepare, 'errors.startPreparation');
        loadDownloads({ quiet: true });
        const res = await apiFetch(`/api/media/info?magnet=${encodeURIComponent(magnet)}&file=${idx}`, { signal: request.signal });
        if (!res.ok) throw await responseError(res, 'errors.mediaInfo');
        const info = await res.json();
        if (isStale(requestId)) return;
        loadDownloads({ quiet: true });

        const audioCount = info.audioTracks ? info.audioTracks.length : 0;
        const subCount = info.subtitles ? info.subtitles.length : 0;
        const audioSelect = $('audioSelect');
        const subSelect = $('subtitleSelect');
        const audioRow = audioSelect.closest('.track-row');
        const subRow = subSelect.closest('.track-row');

        // If nothing to choose, play immediately
        if (audioCount <= 1 && subCount === 0) {
          showMsg('fileMsg', '');
          setOptions(audioSelect, [{ value: 0, label: () => t('player.default') }]);
          setOptions(subSelect, [{ value: -1, label: () => t('player.none') }]);
          if (audioRow) audioRow.style.display = 'none';
          if (subRow) subRow.style.display = 'none';
          $('play').click();
          return;
        }

        // Audio tracks
        if (audioCount > 1) {
          setOptions(audioSelect, info.audioTracks.map(t => ({
            value: t.index,
            label: () => formatTrackLabel(t, 'audio'),
          })));
          if (audioRow) audioRow.style.display = '';
        } else {
          setOptions(audioSelect, [{ value: 0, label: () => t('player.default') }]);
          if (audioRow) audioRow.style.display = 'none';
        }

        // Subtitles
        setOptions(subSelect, [{ value: -1, label: () => t('player.none') }]);
        if (subCount > 0) {
          setOptions(subSelect, [
            { value: -1, label: () => t('player.none') },
            ...info.subtitles.map(t => ({
              value: t.index,
              label: () => formatTrackLabel(t, 'subtitle'),
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
        removeWarning();
        showError('fileMsg', e, 'errors.tracks');
      } finally {
        finishFlowRequest(request);
      }
    };

    $('filelist').addEventListener('change', () => { pendingResume = null; });

    async function startPlayback(resumeTime) {
      const magnet = $('magnet').value.trim();
      const idx = $('filelist').value;
      const audio = $('audioSelect').value || '0';
      const subtitle = $('subtitleSelect').value || '-1';
      const subtitleSelected = parseInt(subtitle, 10) >= 0;
      if (!magnet || idx === undefined || idx === '') {
        showMsg('trackMsg', 'player.selectFile', true);
        return;
      }
      resumeTime ??= pendingResume?.magnet === magnet && pendingResume.fileIndex === Number(idx) ? pendingResume.time : 0;
      const request = beginFlowRequest();
      const requestId = request.id;
      const wasPlaying = $('player-block').style.display !== 'none' || document.body.classList.contains('has-player');
      destroyVideoAndHls({ resetLayout: !wasPlaying });
      $('player-block').style.display = 'none';
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
        return;
      } finally {
        finishFlowRequest(request);
      }
    }

    $('play').onclick = () => startPlayback();
    ['audioSelect', 'subtitleSelect'].forEach(id => {
      const el = $(id);
      if (!el) return;
      el.addEventListener('change', () => {
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
      removeWarning();
      showError('playerMsg', { detail: details }, 'player.playbackError');
    }

    function playHls(src, enableSubtitles, resumeTime = 0) {
      const video = $('video');
      video.style.opacity = 0;
      setTimeout(() => { video.style.opacity = 1; }, 120);
      lastStreamActivityAt = Date.now();
      video.addEventListener('error', () => showPlaybackError('Native media error'));

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
        lastStreamActivityAt = Date.now();
        showMsg('playerMsg', '');
        hideWarningOnce();
      }
      if (typeof Hls !== 'undefined' && Hls.isSupported()) {
        hls = new Hls({ startPosition: resumeTime });
        hls.loadSource(src);
        hls.attachMedia(video);

        hls.on(Hls.Events.MANIFEST_PARSED, () => {
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
            showSubtitleTextTracks(video);
            reapplySubtitleDelay();
          });
          hls.on(Hls.Events.SUBTITLE_TRACK_SWITCH, () => {
            showSubtitleTextTracks(video);
            reapplySubtitleDelay();
          });
        }
        hls.on(Hls.Events.FRAG_LOADED, markStreamActive);
        hls.on(Hls.Events.ERROR, (evt, data) => {
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
    });

    // Main logic
    const $ = id => document.getElementById(id);
    async function apiFetch(input, init) {
      const response = await window.fetch(input, init);
      if (response.status === 401) {
        window.location.replace('/login');
        throw new Error('Session expired');
      }
      return response;
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
    function showWarning() {
      clearSubtitleWait();
      $('warnMsg').hidden = false;
      renderPreparationProgress();
      renderDownloads(downloadCatalog);
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
      clearSubtitleWait();
      $('warnMsg').hidden = true;
      renderDownloads(downloadCatalog);
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

    function formatExpiry(value) {
      const expires = new Date(value);
      if (!Number.isFinite(expires.getTime()) || expires.getUTCFullYear() <= 1) return 'no expiry';
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
      const diskSize = Number.isFinite(download.diskSize) ? download.diskSize : 0;
      return `${formatBytes(diskSize)} / ${formatBytes(download.size)}`;
    }

    function formatDownloadState(download) {
      if (download.status === 'ready') return formatExpiry(download.expiresAt);
      if (download.status === 'preparing') return 'preparing';
      if (download.status === 'failed') return download.preparationError || 'preparation failed';
      if (download.status === 'expired') return 'expired';
      return 'select a video';
    }

    function preparationStage(progress) {
      if (progress.stage === 'playable') return 'Video segments ready · preparing the rest';
      if (progress.stage === 'preparing') return 'Preparing video';
      return 'Downloading initial data and reading video tracks';
    }

    function preparationDetails(progress) {
      const parts = [`Downloaded ${formatBytes(progress.downloadedBytes)} / ${formatBytes(progress.totalBytes)}`];
      if (progress.totalBytes > 0) {
        const percent = Math.min(100, Math.floor(100 * progress.downloadedBytes / progress.totalBytes));
        parts[0] += ` (${percent}%)`;
      }
      if (progress.totalBytes > 0 && progress.downloadedBytes < progress.totalBytes) {
        parts.push(Number.isFinite(progress.downloadSpeed) ? `${formatBytes(progress.downloadSpeed)}/s` : 'Measuring speed…');
        parts.push(`${progress.peers} ${progress.peers === 1 ? 'peer' : 'peers'}`);
      }
      return parts.join(' · ');
    }

    function preparedVideo(progress) {
      const seconds = Math.floor(progress.preparedSeconds);
      if (seconds <= 0) return 'Waiting for the first video segment…';
      const hours = Math.floor(seconds / 3600);
      const minutes = Math.floor(seconds / 60) % 60;
      const clock = `${hours ? `${hours}:${String(minutes).padStart(2, '0')}` : minutes}:${String(seconds % 60).padStart(2, '0')}`;
      return `Video prepared: ${clock}`;
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
      let stage = 'Preparing selected video…';
      let details = '';
      if (matches) {
        stage = preparationStage(progress);
        details = preparationDetails(progress);
      } else if (selected && download.status === 'failed') {
        stage = 'Preparation failed';
        details = download.preparationError || 'Could not prepare video';
      } else if (selected && download.status === 'ready') {
        stage = 'Video is ready to play';
      }
      if (flowRequestController && (matches && progress.stage === 'playable' || selected && download.status === 'ready')) {
        stage = 'Preparing selected stream…';
      }
      if (progressUnavailable) {
        stage = 'Reconnecting to the server…';
        details = 'Progress is unavailable. Retrying…';
      }
      $('preparationStage').textContent = stage;
      const bar = $('preparationProgress');
      bar.hidden = !matches || progressUnavailable || progress.totalBytes <= 0;
      if (!bar.hidden) bar.value = Math.min(1, progress.downloadedBytes / progress.totalBytes);
      $('preparationDetails').textContent = details;
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
        empty.textContent = 'No downloads yet';
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
        row.querySelector('.download-title').textContent = download.title || `Torrent ${download.id.slice(0, 8)}`;
        row.querySelector('.download-subtitle').textContent = download.id;
        row.querySelector('.download-meta-text').textContent = `${formatDownloadSize(download)} · ${formatDownloadState(download)}`;
        row.querySelector('.download-meta-text').classList.remove('has-progress');
        row.querySelector('.download-progress').hidden = true;
        const progressAtPlayer = download.id === activeDownloadID && !$('warnMsg').hidden &&
          download.progress?.fileIndex === Number($('filelist').value);
        if (progressAtPlayer) {
          row.querySelector('.download-meta-text').textContent = progressUnavailable ? 'Reconnecting…' :
            download.progress.stage === 'playable' && !flowRequestController ? 'Ready to play' : 'Preparing';
        } else if (download.progress) {
          const meta = row.querySelector('.download-meta-text');
          meta.classList.add('has-progress');
          meta.textContent = progressUnavailable ? 'Progress unavailable · reconnecting…' :
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
      if (!quiet) showMsg('downloadsMsg', 'Loading downloads...', false, true);
      try {
        const res = await apiFetch('/api/downloads' + (progressOnly ? '?progress=1' : ''));
        if (!res.ok) throw new Error('Could not load downloads');
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
          showMsg('downloadsMsg', e.message || 'Could not load downloads', true);
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

    function openDownload(download) {
      if (!download || !download.magnet || !download.files || download.files.length === 0) {
        showMsg('downloadsMsg', 'Download metadata is incomplete', true);
        return;
      }
      cancelFlowRequest();
      destroyVideoAndHls();
      setActiveDownload(download.id);
      $('magnet').value = download.magnet;
      setFileList(download.files);
      if (Number.isInteger(download.selectedFileIndex)) {
        $('filelist').value = String(download.selectedFileIndex);
      }
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
      const deletingCurrentDownload = activeDownloadID === id;
      showMsg('downloadsMsg', 'Deleting download...', false, true);
      try {
        const res = await apiFetch(`/api/downloads/${encodeURIComponent(id)}`, { method: 'DELETE' });
        if (!res.ok) throw new Error('Could not delete download');
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
        showMsg('downloadsMsg', 'Download deleted');
      } catch (e) {
        showMsg('downloadsMsg', e.message || 'Could not delete download', true);
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
      }
    });
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
        showMsg('magnetMsg', 'Magnet link required', true);
        return;
      }
      setActiveDownload(downloadCatalog.find(download => download.magnet === magnet)?.id || null);
      const request = beginFlowRequest();
      const requestId = request.id;
      destroyVideoAndHls();
      showMsg('magnetMsg', 'Finding peers and loading the file list…', false, true);
      $('filelist').textContent = '';
      $('step-files').style.display = 'none';
      $('step-tracks').style.display = 'none';
      $('player-block').style.display = 'none';
      removeWarning();
      try {
        const res = await apiFetch('/api/torrent/files?magnet=' + encodeURIComponent(magnet), { signal: request.signal });
        if (!res.ok) throw new Error((await res.text()).trim() || 'Server error');
        const files = await res.json();
        if (isStale(requestId)) return;
        if (!files.length) throw new Error('No playable files found in torrent');
        setFileList(files);
        $('step-files').style.display = '';
        showMsg('magnetMsg', '');
        loadDownloads({ quiet: true });
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
      showWarning();
      try {
        const prepare = await apiFetch(`/api/hls/prepare?magnet=${encodeURIComponent(magnet)}&file=${idx}`, {
          method: 'POST',
          signal: request.signal,
        });
        if (!prepare.ok) throw new Error((await prepare.text()).trim() || 'Could not start preparation');
        loadDownloads({ quiet: true });
        const res = await apiFetch(`/api/media/info?magnet=${encodeURIComponent(magnet)}&file=${idx}`, { signal: request.signal });
        if (!res.ok) throw new Error((await res.text()).trim() || 'Could not load media info');
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
        removeWarning();
        showMsg('fileMsg', e.message || 'Error loading tracks', true);
      } finally {
        finishFlowRequest(request);
      }
    };

    async function startPlayback(resumeTime = 0) {
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
      $('player-block').style.display = 'none';
      removeWarning();
      showWarning();
      showMsg('trackMsg', '');
      showMsg('playerMsg', '');
      lastStreamActivityAt = Date.now();

      if (subtitleSelected) {
        clearSubtitleWait();
        subtitleWaitTimer = setTimeout(() => {
          setWarningExtra('Waiting for subtitles...');
        }, 8000);
      }
      try {
        const resp = await apiFetch(`/api/hls/prepare?magnet=${encodeURIComponent(magnet)}&file=${idx}&audio=${audio}&subtitle=${subtitle}`, {
          redirect: 'follow',
          signal: request.signal,
        });
        if (!resp.ok) throw new Error((await resp.text()).trim() || 'Stream error');
        if (isStale(requestId)) return;
        const m3u8 = resp.url.replace(window.location.origin, '') + '?t=' + Date.now();
        $('player-block').style.display = '';
        document.body.classList.add('has-player');
        playHls(m3u8, subtitleSelected, resumeTime);
        loadDownloads({ quiet: true });
      } catch (e) {
        if (e.name === 'AbortError') return;
        if (isStale(requestId)) return;
        removeWarning();
        const msg = e.message || 'Could not start stream';
        showMsg('trackMsg', msg, true);
        return;
      } finally {
        finishFlowRequest(request);
      }
    }

    $('play').onclick = () => startPlayback(0);
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
      showMsg('playerMsg', 'Playback error: ' + (details || 'Fatal error'), true);
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
        video.addEventListener('loadeddata', () => {
          markStreamActive();
          if (enableSubtitles) showSubtitleTextTracks(video);
          reapplySubtitleDelay();
        }, { once: true });
      } else {
        removeWarning();
        showMsg('playerMsg', 'Your browser does not support HLS.', true);
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

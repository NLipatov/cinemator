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
      if (!mode) {
        mode = window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
      }
      setTheme(themes.indexOf(mode), false);
    })();

    // Main logic
    const $ = id => document.getElementById(id);
    let hls = null, files = [];
    let subtitleDelay = parseFloat(localStorage.getItem('subtitle-delay') || '0');
    let msgTimeout = null;
    let subtitleWaitTimer = null;
    function destroyVideoAndHls() {
      if (hls) { hls.destroy(); hls = null; }
      const oldVideo = $('video');
      const newVideo = oldVideo.cloneNode(false);
      oldVideo.parentNode.replaceChild(newVideo, oldVideo);
      newVideo.id = 'video';
    }
    function showMsg(id, msg, isErr=false, loader=false) {
      clearTimeout(msgTimeout);
      const el = $(id);
      el.textContent = '';
      if (loader) el.innerHTML = '<span class="loader"></span>';
      if (msg) el.innerHTML += msg;
      el.className = 'msg' + (isErr ? ' error' : '');
      const shouldAutoClear = msg && !isErr && !loader;
      if (shouldAutoClear) {
        msgTimeout = setTimeout(() => { el.textContent = ''; }, 2200);
      }
    }
    function showWarning() {
      const warn = document.createElement('div');
      warn.className = 'warning';
      warn.id = 'warnMsg';
      warn.innerHTML = `
        <div class="warning-inner">
          <span class="warning-icon">⚠️</span>
          <div class="warning-text">
            <span>Server is downloading and preparing the video.</span>
            <span>This may take several minutes for large torrents.</span>
            <strong>Please stay on this page until playback begins.</strong>
            <span id="warn-extra" style="display:none;"></span>
          </div>
        </div>
      `;
      removeWarning();
      $('warn-container').appendChild(warn);
    }
    function setWarningExtra(msg) {
      const extra = document.getElementById('warn-extra');
      if (!extra) return;
      if (msg) {
        extra.textContent = msg;
        extra.style.display = '';
      } else {
        extra.textContent = '';
        extra.style.display = 'none';
      }
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
      const existing = document.getElementById('warnMsg');
      if (existing && existing.parentNode) existing.parentNode.removeChild(existing);
    }
    $('form').onsubmit = async e => {
      e.preventDefault();
      destroyVideoAndHls();
      showMsg('magnetMsg', 'Loading file list…', false, true);
      $('filelist').innerHTML = '';
      $('step-files').style.display = 'none';
      $('step-tracks').style.display = 'none';
      $('player-block').style.display = 'none';
      removeWarning();
      const magnet = $('magnet').value.trim();
      if (!magnet) return;
      try {
        const res = await fetch('/api/torrent/files?magnet=' + encodeURIComponent(magnet));
        if (!res.ok) throw new Error('Server error');
        files = await res.json();
        if (!files.length) throw new Error('No playable files found in torrent');
        $('filelist').innerHTML = files.map(f =>
          `<option value="${f.index}">${f.name} (${(f.size/1048576).toFixed(2)} MB)</option>`
        ).join('');
        $('step-files').style.display = '';
        showMsg('magnetMsg', '');
      } catch (e) {
        showMsg('magnetMsg', e.message || 'Error loading files', true);
        return;
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
      showMsg('fileMsg', 'Loading track info…', false, true);
      $('step-tracks').style.display = 'none';
      const magnet = $('magnet').value.trim();
      const idx = $('filelist').value;
      if (!magnet || idx === undefined) return;
      try {
        const res = await fetch(`/api/media/info?magnet=${encodeURIComponent(magnet)}&file=${idx}`);
        if (!res.ok) throw new Error('Could not load media info');
        const info = await res.json();

        const audioCount = info.audioTracks ? info.audioTracks.length : 0;
        const subCount = info.subtitles ? info.subtitles.length : 0;
        const audioSelect = $('audioSelect');
        const subSelect = $('subtitleSelect');
        const audioRow = audioSelect.closest('.track-row');
        const subRow = subSelect.closest('.track-row');

        // If nothing to choose, play immediately
        if (audioCount <= 1 && subCount === 0) {
          showMsg('fileMsg', '');
          audioSelect.innerHTML = '<option value="0">Default</option>';
          subSelect.innerHTML = '<option value="-1">None</option>';
          if (audioRow) audioRow.style.display = 'none';
          if (subRow) subRow.style.display = 'none';
          $('play').click();
          return;
        }

        // Audio tracks
        if (audioCount > 1) {
          audioSelect.innerHTML = info.audioTracks.map(t =>
            `<option value="${t.index}">${formatTrackLabel(t, 'audio')}</option>`
          ).join('');
          if (audioRow) audioRow.style.display = '';
        } else {
          audioSelect.innerHTML = '<option value="0">Default</option>';
          if (audioRow) audioRow.style.display = 'none';
        }

        // Subtitles
        subSelect.innerHTML = '<option value="-1">None</option>';
        if (subCount > 0) {
          subSelect.innerHTML += info.subtitles.map(t =>
            `<option value="${t.index}">${formatTrackLabel(t, 'subtitle')}</option>`
          ).join('');
          if (subRow) subRow.style.display = '';
          $('subtitleDelayRow').style.display = '';
        } else {
          if (subRow) subRow.style.display = 'none';
          $('subtitleDelayRow').style.display = 'none';
        }

        $('step-tracks').style.display = '';
        showMsg('fileMsg', '');
      } catch (e) {
        showMsg('fileMsg', e.message || 'Error loading tracks', true);
      }
    };

    async function startPlayback(resumeTime = 0) {
      destroyVideoAndHls();
      $('player-block').style.display = 'none';
      removeWarning();
      showWarning();
      showMsg('trackMsg', '');

      const magnet = $('magnet').value.trim();
      const idx = $('filelist').value;
      const audio = $('audioSelect').value || '0';
      const subtitle = $('subtitleSelect').value || '-1';
      const subtitleSelected = parseInt(subtitle, 10) >= 0;
      if (!magnet || idx === undefined) return;
      if (subtitleSelected) {
        clearSubtitleWait();
        subtitleWaitTimer = setTimeout(() => {
          setWarningExtra('Waiting for subtitles...');
        }, 8000);
      }
      try {
        const resp = await fetch(`/api/hls/prepare?magnet=${encodeURIComponent(magnet)}&file=${idx}&audio=${audio}&subtitle=${subtitle}`, { redirect: 'follow' });
        if (!resp.ok) throw new Error('Stream error');
        const m3u8 = resp.url.replace(window.location.origin, '') + '?t=' + Date.now();
        $('player-block').style.display = '';
        playHls(m3u8, subtitleSelected, resumeTime);
      } catch (e) {
        removeWarning();
        showMsg('trackMsg', e.message || 'Could not start stream', true);
        return;
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
        if (!cue.__origStart) {
          cue.__origStart = cue.startTime;
          cue.__origEnd = cue.endTime;
        }
        cue.startTime = Math.max(0, cue.__origStart + delaySec);
        cue.endTime = Math.max(cue.startTime, cue.__origEnd + delaySec);
      }
    }

    function playHls(src, enableSubtitles, resumeTime = 0) {
      const video = $('video');
      video.style.opacity = 0;
      setTimeout(() => { video.style.opacity = 1; }, 120);

      let fragLoaded = false;
      function hideWarningOnce() {
        if (!fragLoaded) {
          fragLoaded = true;
          removeWarning();
        }
      }

      if (Hls.isSupported()) {
        hls = new Hls();
        hls.loadSource(src);
        hls.attachMedia(video);

        hls.on(Hls.Events.MANIFEST_PARSED, () => {
          if (resumeTime > 0) {
            video.currentTime = resumeTime;
          }
          if (enableSubtitles && hls.subtitleTracks && hls.subtitleTracks.length > 0) {
            hls.subtitleTrack = 0;
            hls.subtitleDisplay = true;
            applySubtitleDelay(video, subtitleDelay);
          }
        });
        hls.on(Hls.Events.FRAG_LOADED, hideWarningOnce);
        hls.on(Hls.Events.ERROR, (evt, data) => {
          if (data.fatal) {
            removeWarning();
            showMsg('playerMsg', 'Playback error: ' + (data.details || 'Fatal error'), true);
          }
        });
      } else if (video.canPlayType('application/vnd.apple.mpegurl')) {
        video.src = src;
        if (resumeTime > 0) {
          video.addEventListener('loadedmetadata', () => {
            video.currentTime = resumeTime;
          }, { once: true });
        }
        video.addEventListener('canplay', hideWarningOnce, { once: true });
        video.addEventListener('loadeddata', () => applySubtitleDelay(video, subtitleDelay), { once: true });
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

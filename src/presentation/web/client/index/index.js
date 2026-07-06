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
    let hls = null;
    let subtitleDelay = parseFloat(localStorage.getItem('subtitle-delay') || '0');
    let msgTimeout = null;
    let subtitleWaitTimer = null;
    let playbackRecoveryTimer = null;
    let playbackRecoveryInFlight = false;
    let lastPlaybackRecoveryAt = 0;
    let lastStreamActivityAt = 0;
    let requestSeq = 0;
    const idleRecoveryMs = 12 * 60 * 1000;
    const recoveryThrottleMs = 5000;
    const stallRecoveryDelayMs = 3500;
    const nextRequest = () => ++requestSeq;
    const isStale = id => id !== requestSeq;
    function destroyVideoAndHls({ resetLayout = true } = {}) {
      if (hls) { hls.destroy(); hls = null; }
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
    }
    $('form').onsubmit = async e => {
      e.preventDefault();
      const magnet = $('magnet').value.trim();
      if (!magnet) {
        showMsg('magnetMsg', 'Magnet link required', true);
        return;
      }
      const requestId = nextRequest();
      destroyVideoAndHls();
      showMsg('magnetMsg', 'Loading file list…', false, true);
      $('filelist').textContent = '';
      $('step-files').style.display = 'none';
      $('step-tracks').style.display = 'none';
      $('player-block').style.display = 'none';
      removeWarning();
      try {
        const res = await fetch('/api/torrent/files?magnet=' + encodeURIComponent(magnet));
        if (!res.ok) throw new Error('Server error');
        const files = await res.json();
        if (isStale(requestId)) return;
        if (!files.length) throw new Error('No playable files found in torrent');
        setOptions($('filelist'), files.map(f => ({
          value: f.index,
          label: `${f.name} (${(f.size/1048576).toFixed(2)} MB)`,
        })));
        $('step-files').style.display = '';
        showMsg('magnetMsg', '');
      } catch (e) {
        if (isStale(requestId)) return;
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
      const magnet = $('magnet').value.trim();
      const idx = $('filelist').value;
      if (!magnet || idx === undefined || idx === '') {
        showMsg('fileMsg', 'Select a file first', true);
        return;
      }
      const requestId = nextRequest();
      showMsg('fileMsg', 'Loading track info…', false, true);
      $('step-tracks').style.display = 'none';
      try {
        const res = await fetch(`/api/media/info?magnet=${encodeURIComponent(magnet)}&file=${idx}`);
        if (!res.ok) throw new Error('Could not load media info');
        const info = await res.json();
        if (isStale(requestId)) return;

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
        if (isStale(requestId)) return;
        showMsg('fileMsg', e.message || 'Error loading tracks', true);
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
      const requestId = nextRequest();
      const wasPlaying = $('player-block').style.display !== 'none' || document.body.classList.contains('has-player');
      destroyVideoAndHls({ resetLayout: !wasPlaying });
      $('player-block').style.display = keepPlayerVisible ? '' : 'none';
      removeWarning();
      showWarning();
      showMsg('trackMsg', '');
      showMsg('playerMsg', keepPlayerVisible ? 'Restoring stream...' : '', false, keepPlayerVisible);
      lastStreamActivityAt = Date.now();

      if (subtitleSelected) {
        clearSubtitleWait();
        subtitleWaitTimer = setTimeout(() => {
          setWarningExtra('Waiting for subtitles...');
        }, 8000);
      }
      try {
        const resp = await fetch(`/api/hls/prepare?magnet=${encodeURIComponent(magnet)}&file=${idx}&audio=${audio}&subtitle=${subtitle}`, { redirect: 'follow' });
        if (!resp.ok) throw new Error('Stream error');
        if (isStale(requestId)) return;
        const m3u8 = resp.url.replace(window.location.origin, '') + '?t=' + Date.now();
        $('player-block').style.display = '';
        document.body.classList.add('has-player');
        playHls(m3u8, subtitleSelected, resumeTime);
      } catch (e) {
        if (isStale(requestId)) return;
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

    function attachPlaybackRecovery(video) {
      video.addEventListener('play', () => requestPlaybackRecovery('play'));
      video.addEventListener('seeking', () => requestPlaybackRecovery('seeking'));
      video.addEventListener('waiting', () => schedulePlaybackRecovery('waiting'));
      video.addEventListener('stalled', () => schedulePlaybackRecovery('stalled'));
      video.addEventListener('error', () => requestPlaybackRecovery('native-error', true));
    }

    function playHls(src, enableSubtitles, resumeTime = 0) {
      const video = $('video');
      video.style.opacity = 0;
      setTimeout(() => { video.style.opacity = 1; }, 120);
      lastStreamActivityAt = Date.now();
      attachPlaybackRecovery(video);

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

      if (Hls.isSupported()) {
        hls = new Hls();
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
            removeWarning();
            if (requestPlaybackRecovery('hls-error', true)) return;
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
        video.addEventListener('canplay', markStreamActive, { once: true });
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

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
    let msgTimeout = null;
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
      if (msg && !isErr) {
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
          </div>
        </div>
      `;
      removeWarning();
      $('warn-container').appendChild(warn);
    }
    function removeWarning() {
      const existing = document.getElementById('warnMsg');
      if (existing && existing.parentNode) existing.parentNode.removeChild(existing);
    }
    $('form').onsubmit = async e => {
      e.preventDefault();
      destroyVideoAndHls();
      clearAudioSelector();
      showMsg('magnetMsg', 'Loading file list…', false, true);
      $('filelist').innerHTML = '';
      $('step-files').style.display = 'none';
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
    $('play').onclick = async () => {
      disablePlayButton();
      destroyVideoAndHls();
      clearAudioSelector();
      $('player-block').style.display = 'none';
      removeWarning();
      showWarning();
      showMsg('fileMsg', '');

      const magnet = $('magnet').value.trim();
      const idx = $('filelist').value;
      if (!magnet || idx === undefined) return;
      try {
        const resp = await fetch(
            `/api/hls/prepare?magnet=${encodeURIComponent(magnet)}&file=${idx}`,
            { redirect: 'follow' }
        );
        
        // Checking Content-Type to distinct json with audiotracks from hls stream start
        const ct = resp.headers.get("content-type") || "";
        if (ct.includes("application/json")) {
          const tracks = await resp.json();
          // tracks: [{Title, Language, ...}]
          showAudioSelector(tracks, async (audioIdx) => {
            // second play request with selected audiotrack
            const audioTrackPickResponse = await fetch(
                `/api/hls/prepare?magnet=${encodeURIComponent(magnet)}&file=${idx}&audio=${audioIdx}`,
                { redirect: 'follow' }
            );
            if (!audioTrackPickResponse.ok) {
                enablePlayButton();
                throw new Error('Stream error');
            }
            const m3u8 = audioTrackPickResponse.url.replace(window.location.origin, '') + '?t=' + Date.now();
            $('player-block').style.display = '';
            playHls(m3u8);
          });
          return;
        }
        
        if (!resp.ok) throw new Error('Stream error');
        const m3u8 = resp.url.replace(window.location.origin, '') + '?t=' + Date.now();
        $('player-block').style.display = '';
        playHls(m3u8);
      } catch (e) {
        removeWarning();
        showMsg('fileMsg', e.message || 'Could not start stream', true);
        disablePlayButton();
        return;
      }
    };

    function playHls(src) {
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

        hls.on(Hls.Events.FRAG_LOADED, hideWarningOnce);
        hls.on(Hls.Events.ERROR, (evt, data) => {
          if (data.fatal) {
            removeWarning();
            showMsg('playerMsg', 'Playback error: ' + (data.details || 'Fatal error'), true);
          }
        });
      } else if (video.canPlayType('application/vnd.apple.mpegurl')) {
        video.src = src;
        video.addEventListener('canplay', hideWarningOnce, { once: true });
      } else {
        removeWarning();
        showMsg('playerMsg', 'Your browser does not support HLS.', true);
      }
    }
    
    function showAudioSelector(tracks, cb) {
      $('audio-selector').innerHTML = `
        <label style="font-weight:500;display:block;margin-bottom:5px">Select audio track:</label>
        <select id="audioTrackSelect" class="input-style" style="margin-bottom:8px;">
          ${tracks.map((t,i) =>
            `<option value="${i}">${t.title || 'Track '+(i+1)}${t.language ? ' ('+t.language+')' : ''}</option>`
          ).join('')}
        </select>
        <button id="audioSelectBtn" class="input-style" style="margin-left:10px;">OK</button>
      `;
      $('audioSelectBtn').onclick = () => {
        const idx = $('audioTrackSelect').value;
        cb(idx);
        $('play').disabled = false;
        clearAudioSelector();
        enablePlayButton();
      };
    }
    
    function clearAudioSelector() {
      $('audio-selector').innerHTML = '';
    }
    
    function disablePlayButton() {
      const playBtn = document.getElementById('play');
      if (playBtn) playBtn.disabled = true;
    }
    
    function enablePlayButton() {
      const playBtn = document.getElementById('play');
      if (playBtn) playBtn.disabled = false;
    }
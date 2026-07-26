/* TwitchCaster channel list.
 *
 * The server answers /gui/cast/ with 200 and an empty body *before* it does any
 * work — the cast runs in a goroutine — so the response says nothing about
 * whether a stream actually started. Confirmation instead comes from polling
 * /current-stream/{ip-with-dashes}, which the Go side publishes only once the
 * pipeline is up and segments exist. That is the earliest honest "the picture
 * is arriving" signal available.
 */
(function () {
  'use strict';

  var CAST_TIMEOUT_MS = 45000;   // generous: streamlink + ffmpeg startup on a Pi
  var POLL_MS = 1000;
  var STALE_MS = 60000;          // hidden longer than this → refresh on return
  var ERROR_CLEAR_MS = 8000;
  var ROOM_KEY = 'twitchcaster.room';

  var CAST_URL = document.body.dataset.castUrl || '/gui/cast/';

  var roomsEl = document.getElementById('rooms');
  var onairEl = document.getElementById('onair');
  var onairText = document.getElementById('onairText');
  var stopBtn = document.getElementById('stopBtn');
  var finderEl = document.getElementById('finder');
  var finderInput = document.getElementById('finderInput');
  var finderToggle = document.getElementById('finderToggle');
  var finderClear = document.getElementById('finderClear');
  var manualEl = document.getElementById('manual');
  var refreshBtn = document.getElementById('refreshBtn');
  var mainEl = document.querySelector('main');

  // Bumping this cancels any in-flight confirmation poll, so tapping a second
  // card (or Stop) never lets a stale poll overwrite the newer state.
  var castGen = 0;
  var onAirByIP = {};   // device IP → login currently published for it
  var hiddenAt = 0;
  var errorTimer = null;

  /* ── helpers ─────────────────────────────────────────────────── */

  function sleep(ms) { return new Promise(function (r) { setTimeout(r, ms); }); }
  function hlsID(ip) { return ip.replace(/\./g, '-'); }
  function cards() { return Array.prototype.slice.call(document.querySelectorAll('.card')); }
  function roomButtons() { return Array.prototype.slice.call(roomsEl.querySelectorAll('.room')); }
  function selectedRoom() { return roomsEl.querySelector('.room[aria-checked="true"]'); }

  function buzz(pattern) {
    try { if (navigator.vibrate) navigator.vibrate(pattern); } catch (e) { /* unsupported */ }
  }

  function currentStream(ip) {
    return fetch('/current-stream/' + hlsID(ip), { cache: 'no-store' })
      .then(function (res) { return res.ok && res.status !== 204 ? res.json() : null; })
      .catch(function () { return null; });
  }

  /* ── rooms ───────────────────────────────────────────────────── */

  function selectRoom(btn) {
    roomButtons().forEach(function (b) { b.setAttribute('aria-checked', String(b === btn)); });
    try { localStorage.setItem(ROOM_KEY, btn.dataset.ip); } catch (e) { /* private mode */ }
    clearTransmitStates();
    syncCastUI();
    applyFilter();
  }

  function restoreRoom() {
    var buttons = roomButtons();
    if (!buttons.length) return;
    var saved = null;
    try { saved = localStorage.getItem(ROOM_KEY); } catch (e) { /* private mode */ }
    var match = buttons.filter(function (b) { return b.dataset.ip === saved; })[0];
    (match || buttons[0]).setAttribute('aria-checked', 'true');
  }

  /* ── card state ──────────────────────────────────────────────── */

  function setBadge(card, text) {
    var label = card.querySelector('.badge-label');
    if (label) label.textContent = text;
  }

  function setCardState(card, state, text) {
    card.classList.remove('is-sending', 'is-onair', 'is-error');
    if (state) card.classList.add('is-' + state);
    var status = card.querySelector('.status');
    if (status) status.textContent = text || '';
  }

  function clearTransmitStates() {
    if (errorTimer) { clearTimeout(errorTimer); errorTimer = null; }
    document.querySelectorAll('.grid').forEach(function (g) { g.classList.remove('is-transmitting'); });
    cards().forEach(function (c) { c.classList.remove('is-sending', 'is-error'); });
  }

  // Rebuilds every piece of cast UI from onAirByIP + the selected room, so the
  // same code path serves initial load, a successful cast, Stop, and a refresh.
  function syncCastUI() {
    roomButtons().forEach(function (b) {
      b.classList.toggle('is-casting', !!onAirByIP[b.dataset.ip]);
    });

    var room = selectedRoom();
    var login = room ? onAirByIP[room.dataset.ip] : null;

    cards().forEach(function (c) {
      if (c.classList.contains('is-sending') || c.classList.contains('is-error')) return;
      var on = !!login && c.dataset.login === login;
      c.classList.toggle('is-onair', on);
      setBadge(c, on ? 'On air · ' + room.dataset.name : 'Live');
    });

    if (!login || !room) {
      onairEl.hidden = true;
      return;
    }
    var card = cards().filter(function (c) { return c.dataset.login === login; })[0];
    onairText.textContent = (card ? card.dataset.name : login) + ' · ';
    var where = document.createElement('span');
    where.className = 'room-name';
    where.textContent = room.dataset.name;
    onairText.appendChild(where);
    onairEl.hidden = false;
  }

  function probeRooms() {
    return Promise.all(roomButtons().map(function (b) {
      return currentStream(b.dataset.ip).then(function (s) {
        if (s && s.login) onAirByIP[b.dataset.ip] = s.login;
        else delete onAirByIP[b.dataset.ip];
      });
    })).then(syncCastUI);
  }

  /* ── casting ─────────────────────────────────────────────────── */

  // Manual casts have no card to annotate, so their progress and failures are
  // reported in the finder's own row instead.
  function setManualStatus(text, isError) {
    if (!text) { renderManual(finderInput.value.trim()); return; }
    manualEl.textContent = '';
    var msg = document.createElement('p');
    msg.className = 'manual-msg' + (isError ? ' is-error' : '');
    msg.textContent = text;
    manualEl.appendChild(msg);
    manualEl.hidden = false;
  }

  function failCast(card, roomName, message) {
    document.querySelectorAll('.grid').forEach(function (g) { g.classList.remove('is-transmitting'); });
    if (!card) {
      setManualStatus(message, true);
      errorTimer = setTimeout(function () { setManualStatus(''); }, ERROR_CLEAR_MS);
      return;
    }
    setCardState(card, 'error', message);
    errorTimer = setTimeout(function () {
      setCardState(card, null, '');
      syncCastUI();
    }, ERROR_CLEAR_MS);
  }

  function castChannel(login, card) {
    var room = selectedRoom();
    if (!room || !login) return;
    var ip = room.dataset.ip;
    var roomName = room.dataset.name;
    var gen = ++castGen;

    buzz(15);
    clearTransmitStates();
    if (card) {
      card.closest('.grid').classList.add('is-transmitting');
      setCardState(card, 'sending', 'Sending → ' + roomName);
    } else {
      setManualStatus('Sending ' + login + ' → ' + roomName, false);
    }

    // Remember what the device was publishing first: the seq comparison below
    // is what lets a re-cast of the channel already playing there still confirm.
    currentStream(ip).then(function (before) {
      if (gen !== castGen) return;
      return fetch(CAST_URL + encodeURIComponent(login) + '/' + encodeURIComponent(ip), { cache: 'no-store' })
        .then(function (res) {
          if (gen !== castGen) return;
          if (!res.ok) {
            failCast(card, roomName, res.status === 404
              ? roomName + ' is not a known device'
              : 'Cast request failed (' + res.status + ') · tap to retry');
            return;
          }
          return confirmCast(login, ip, roomName, card, before, gen);
        }, function () {
          if (gen !== castGen) return;
          failCast(card, roomName, 'Server unreachable · tap to retry');
        });
    });
  }

  function confirmCast(login, ip, roomName, card, before, gen) {
    var deadline = Date.now() + CAST_TIMEOUT_MS;

    function tick() {
      if (gen !== castGen) return;
      if (Date.now() > deadline) {
        failCast(card, roomName, 'No picture on ' + roomName + ' · tap to retry');
        return;
      }
      return sleep(POLL_MS).then(function () {
        if (gen !== castGen) return;
        return currentStream(ip);
      }).then(function (s) {
        if (gen !== castGen) return;
        if (s && s.login === login && (!before || s.seq > before.seq)) {
          buzz([12, 55, 12]);
          onAirByIP[ip] = login;
          document.querySelectorAll('.grid').forEach(function (g) { g.classList.remove('is-transmitting'); });
          if (card) setCardState(card, null, '');
          else setManualStatus('');
          syncCastUI();
          return;
        }
        return tick();
      });
    }
    return tick();
  }

  function stopCast() {
    var room = selectedRoom();
    if (!room) return;
    castGen++;                       // cancel any confirmation still polling
    var ip = room.dataset.ip;
    stopBtn.disabled = true;
    fetch('/stop-cast/' + hlsID(ip), { cache: 'no-store' })
      .catch(function () { /* the watchdog will clean up regardless */ })
      .then(function () {
        delete onAirByIP[ip];
        stopBtn.disabled = false;
        clearTransmitStates();
        cards().forEach(function (c) { c.classList.remove('is-onair'); });
        syncCastUI();
      });
  }

  /* ── finder: filters the list, and casts what it can't find ──── */

  // Offered only when the filter turns up nothing — while there are matches the
  // user is browsing, not naming a channel, and the row would just be noise.
  function renderManual(query, matches) {
    var room = selectedRoom();
    var valid = /^[a-zA-Z0-9_]{2,25}$/.test(query);

    manualEl.textContent = '';
    if (!valid || matches > 0 || !room) { manualEl.hidden = true; return; }

    var btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'manual-btn';
    btn.appendChild(document.createTextNode('Cast '));
    var name = document.createElement('strong');
    name.textContent = query;
    btn.appendChild(name);
    btn.appendChild(document.createTextNode(' to ' + room.dataset.name));
    var arrow = document.createElement('span');
    arrow.className = 'arrow';
    arrow.textContent = '→';
    btn.appendChild(arrow);
    btn.addEventListener('click', function () { castChannel(query.toLowerCase(), null); });

    manualEl.appendChild(btn);
    manualEl.hidden = false;
  }

  function applyFilter() {
    var query = finderInput.value.trim();
    var q = query.toLowerCase();
    var all = cards();
    var shown = 0;

    all.forEach(function (c) {
      var hay = (c.dataset.login + ' ' + c.dataset.name + ' ' + c.dataset.game).toLowerCase();
      var hit = !q || hay.indexOf(q) !== -1;
      c.classList.toggle('is-hidden', !hit);
      if (hit) shown++;
    });

    renderManual(query, shown);

    // The manual row already says what to do about an empty result, so don't
    // say it twice.
    var noMatch = document.getElementById('noMatch');
    if (noMatch) noMatch.hidden = !(q && shown === 0 && all.length > 0 && manualEl.hidden);
  }

  function toggleFinder(open) {
    finderEl.hidden = !open;
    finderToggle.setAttribute('aria-expanded', String(open));
    if (open) {
      finderInput.focus();
    } else {
      finderInput.value = '';
      applyFilter();
    }
  }

  /* ── uptime ──────────────────────────────────────────────────── */

  function formatUptime(iso) {
    if (!iso) return '';
    var started = Date.parse(iso);
    if (isNaN(started)) return '';
    var minutes = Math.floor((Date.now() - started) / 60000);
    if (minutes < 0) return '';
    if (minutes < 60) return minutes + 'm';
    return Math.floor(minutes / 60) + 'h ' + (minutes % 60) + 'm';
  }

  function renderUptimes() {
    document.querySelectorAll('.uptime').forEach(function (el) {
      el.textContent = formatUptime(el.dataset.started);
    });
  }

  /* ── refresh ─────────────────────────────────────────────────── */

  var refreshing = false;

  function refreshList() {
    if (refreshing) return Promise.resolve();
    refreshing = true;
    refreshBtn.classList.add('is-spinning');

    return fetch(location.href, { cache: 'no-store', headers: { Accept: 'text/html' } })
      .then(function (res) {
        // A 401 login page or the /auth/twitch bounce means the session or the
        // Twitch token needs attention — hand the whole page over to the server.
        if (!res.ok || res.redirected) { location.reload(); return null; }
        return res.text();
      })
      .then(function (html) {
        if (html === null) return;
        var doc = new DOMParser().parseFromString(html, 'text/html');
        var freshMain = doc.querySelector('main');
        if (!freshMain) { location.reload(); return; }

        castGen++;                    // any in-flight confirm refers to dead nodes
        mainEl.replaceChildren.apply(
          mainEl,
          Array.prototype.slice.call(document.importNode(freshMain, true).childNodes)
        );

        renderUptimes();
        applyFilter();
        return probeRooms();
      })
      .catch(function () { /* keep the list we already have */ })
      .then(function () {
        refreshing = false;
        refreshBtn.classList.remove('is-spinning');
      });
  }

  /* ── wiring ──────────────────────────────────────────────────── */

  roomsEl.addEventListener('click', function (e) {
    var btn = e.target.closest('.room');
    if (btn) selectRoom(btn);
  });

  // Delegated so the handlers survive a refresh swapping out <main>.
  function closestFrom(e, selector) {
    return e.target && e.target.closest ? e.target.closest(selector) : null;
  }

  document.addEventListener('click', function (e) {
    if (closestFrom(e, '#emptyRefresh')) { refreshList(); return; }
    var card = closestFrom(e, '.card');
    if (card) castChannel(card.dataset.login, card);
  });

  document.addEventListener('keydown', function (e) {
    var card = closestFrom(e, '.card');
    if (card && (e.key === 'Enter' || e.key === ' ')) {
      e.preventDefault();
      castChannel(card.dataset.login, card);
    }
  });

  // Press feedback lands before any network call, so the tap registers
  // instantly even when the server is slow. Cleanup listeners are bound to the
  // card itself so the state can't stick when the finger slides off.
  document.addEventListener('pointerdown', function (e) {
    var card = closestFrom(e, '.card');
    if (!card) return;
    card.classList.add('is-pressed');
    var events = ['pointerup', 'pointercancel', 'pointerleave'];
    var release = function () {
      card.classList.remove('is-pressed');
      events.forEach(function (evt) { card.removeEventListener(evt, release); });
    };
    events.forEach(function (evt) { card.addEventListener(evt, release); });
  });

  stopBtn.addEventListener('click', stopCast);
  refreshBtn.addEventListener('click', function () { refreshList(); });
  finderToggle.addEventListener('click', function () { toggleFinder(finderEl.hidden); });
  finderClear.addEventListener('click', function () { toggleFinder(false); });
  finderInput.addEventListener('input', applyFilter);

  finderInput.addEventListener('keydown', function (e) {
    if (e.key === 'Escape') { toggleFinder(false); return; }
    if (e.key !== 'Enter') return;
    e.preventDefault();
    var visible = cards().filter(function (c) { return !c.classList.contains('is-hidden'); });
    var typed = finderInput.value.trim();
    if (visible.length === 1) castChannel(visible[0].dataset.login, visible[0]);
    else if (!manualEl.hidden) castChannel(typed.toLowerCase(), null);
  });

  document.addEventListener('visibilitychange', function () {
    if (document.hidden) { hiddenAt = Date.now(); return; }
    if (hiddenAt && Date.now() - hiddenAt > STALE_MS) refreshList();
    else probeRooms();
  });

  restoreRoom();
  renderUptimes();
  probeRooms();
  // With nobody live, casting by name is the only thing left to do — so put the
  // field in front of the user rather than behind a button.
  if (!cards().length) toggleFinder(true);
  setInterval(renderUptimes, 60000);
})();

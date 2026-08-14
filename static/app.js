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
  var STALE_MS = 60000;          // list older than this → refresh on return
  var REFRESH_TIMEOUT_MS = 10000; // generous: the server calls Twitch to render
  var PROBE_MIN_GAP_MS = 5000;   // focus can fire in bursts; don't re-probe each time
  var RETRY_BASE_MS = 5000;      // first retry after a failed refresh
  var RETRY_MAX_MS = 60000;      // backoff ceiling while the server stays unreachable
  var ERROR_CLEAR_MS = 8000;
  var SEARCH_DEBOUNCE_MS = 350;  // typing settles before we bother Twitch
  var SEARCH_MIN_CHARS = 3;      // the server rejects anything shorter
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
  var searchEl = document.getElementById('searchResults');
  var searchGrid = document.getElementById('searchGrid');
  var searchStatus = document.getElementById('searchStatus');
  var searchTitle = searchEl.querySelector('.section-title');

  // Bumping this cancels any in-flight confirmation poll, so tapping a second
  // card (or Stop) never lets a stale poll overwrite the newer state.
  var castGen = 0;
  var onAirByIP = {};   // device IP → login currently published for it
  var lastProbeAt = 0;
  var errorTimer = null;

  // Same idea as castGen, for searches: only the newest one may render.
  var searchGen = 0;
  var searchTimer = null;
  var searchAbort = null;
  var searchCache = {};   // query → fragment HTML, so backspacing is instant

  /* ── helpers ─────────────────────────────────────────────────── */

  function sleep(ms) { return new Promise(function (r) { setTimeout(r, ms); }); }
  function hlsID(ip) { return ip.replace(/\./g, '-'); }
  // cards() is every card on the page — followed and searched alike — so cast
  // state (sending / on air / error) lands on whichever one was tapped. The
  // finder's text filter only ever applies to localCards().
  function cards() { return Array.prototype.slice.call(document.querySelectorAll('.card')); }
  function localCards() { return Array.prototype.slice.call(document.querySelectorAll('#grid .card')); }
  function searchCards() { return Array.prototype.slice.call(searchGrid.querySelectorAll('.card')); }
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
    lastProbeAt = Date.now();
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
    if (!text) { renderManual(); return; }
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
      // Every grid, not just the card's own: with search results on screen the
      // followed list would otherwise stay bright beside the sending card.
      document.querySelectorAll('.grid').forEach(function (g) { g.classList.add('is-transmitting'); });
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

  function visibleLocalCards() {
    return localCards().filter(function (c) { return !c.classList.contains('is-hidden'); });
  }

  // Offered only when neither the filter nor the Twitch search turned up the
  // typed channel — while there are matches the user is browsing, not naming a
  // channel, and the row would just be noise. It stays reachable for offline
  // channels, which the live-only search never returns.
  function renderManual() {
    var query = finderInput.value.trim();
    var room = selectedRoom();
    var valid = /^[a-zA-Z0-9_]{2,25}$/.test(query);
    var matches = visibleLocalCards().length;
    var searched = searchCards().some(function (c) {
      return c.dataset.login === query.toLowerCase();
    });

    manualEl.textContent = '';
    if (!valid || matches > 0 || searched || !room) { manualEl.hidden = true; return; }

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
    var all = localCards();
    var shown = 0;

    all.forEach(function (c) {
      var hay = (c.dataset.login + ' ' + c.dataset.name + ' ' + c.dataset.game).toLowerCase();
      var hit = !q || hay.indexOf(q) !== -1;
      c.classList.toggle('is-hidden', !hit);
      if (hit) shown++;
    });

    scheduleSearch(query);
    renderFallbacks(shown, all.length, q);
  }

  // The manual row and the Twitch results both answer "the list doesn't have
  // it", so the plain no-match line only speaks when neither of them does.
  function renderFallbacks(shown, total, q) {
    renderManual();
    var noMatch = document.getElementById('noMatch');
    if (noMatch) {
      noMatch.hidden = !(q && shown === 0 && total > 0 && manualEl.hidden && !searchCards().length);
    }
  }

  /* ── Twitch search: what the local list couldn't answer ───────── */

  // The heading names a group of cards, so it only appears once there are some.
  function setSearchStatus(text) {
    searchStatus.textContent = text || '';
    searchStatus.hidden = !text;
    searchTitle.hidden = !searchCards().length;
  }

  function clearSearch() {
    searchGen++;
    if (searchTimer) { clearTimeout(searchTimer); searchTimer = null; }
    if (searchAbort) { searchAbort.abort(); searchAbort = null; }
    searchGrid.textContent = '';
    setSearchStatus('');
    searchEl.hidden = true;
  }

  function scheduleSearch(query) {
    if (searchTimer) { clearTimeout(searchTimer); searchTimer = null; }
    if (query.length < SEARCH_MIN_CHARS) { clearSearch(); return; }

    var cached = searchCache[query.toLowerCase()];
    if (cached !== undefined) {
      // Claim the generation so a slower request still in flight can't land on
      // top of what we're about to render.
      searchGen++;
      if (searchAbort) { searchAbort.abort(); searchAbort = null; }
      renderSearch(cached, query);
      return;
    }

    searchTimer = setTimeout(function () { runSearch(query); }, SEARCH_DEBOUNCE_MS);
  }

  function runSearch(query) {
    var gen = ++searchGen;
    if (searchAbort) searchAbort.abort();
    searchAbort = typeof AbortController === 'function' ? new AbortController() : null;

    searchEl.hidden = false;
    setSearchStatus('Searching Twitch…');

    fetch('/gui/search?q=' + encodeURIComponent(query), {
      cache: 'no-store',
      signal: searchAbort ? searchAbort.signal : undefined
    }).then(function (res) {
      if (!res.ok) throw new Error('search failed: ' + res.status);
      return res.text();
    }).then(function (html) {
      if (gen !== searchGen) return;
      searchCache[query.toLowerCase()] = html;
      renderSearch(html, query);
    }).catch(function (err) {
      // An aborted request is a newer search taking over, not a failure.
      if (gen !== searchGen || (err && err.name === 'AbortError')) return;
      searchGrid.textContent = '';
      setSearchStatus('Twitch search is unavailable right now.');
      searchEl.hidden = false;
    });
  }

  function renderSearch(html, query) {
    var doc = new DOMParser().parseFromString(html, 'text/html');
    var found = Array.prototype.slice.call(doc.querySelectorAll('.card'));

    // Anyone already in the followed list is on screen above; showing them
    // twice just makes the results look padded.
    var known = {};
    localCards().forEach(function (c) { known[c.dataset.login] = true; });

    searchGrid.textContent = '';
    found.forEach(function (c) {
      if (known[c.dataset.login]) return;
      searchGrid.appendChild(document.importNode(c, true));
    });

    var count = searchCards().length;
    var localMatches = visibleLocalCards().length;

    // Coming up empty is only worth saying when the list didn't answer either;
    // otherwise the user already has what they were looking for.
    if (!count && localMatches) {
      searchEl.hidden = true;
      setSearchStatus('');
    } else {
      searchEl.hidden = false;
      setSearchStatus(count ? '' : 'Nobody live on Twitch matches “' + query + '”.');
    }

    renderUptimes();
    syncCastUI();
    renderFallbacks(localMatches, localCards().length, query.toLowerCase());
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
  // When the list was last known good. The server stamps its render time into
  // <body>, so HTML replayed from the browser's HTTP cache ages correctly
  // instead of counting as fresh just because this script re-ran. Clamped to
  // now: a server clock running ahead must not push freshness into the future.
  var listFreshAt = Math.min(Date.now(),
    parseInt(document.body.dataset.renderedAt, 10) || Date.now());

  var retryDelayMs = RETRY_BASE_MS;
  var retryTimer = null;

  // A refresh that fails (wifi still re-associating after wake, server mid-
  // restart) used to fail silently and stay failed until the next lifecycle
  // event. Retry on a doubling backoff while the page is visible; a successful
  // refresh resets it. A retry that lands while hidden just skips — the
  // visibilitychange on return triggers the refetch instead.
  function scheduleRetry() {
    if (retryTimer) return;
    retryTimer = setTimeout(function () {
      retryTimer = null;
      if (!document.hidden) refreshList();
    }, retryDelayMs);
    retryDelayMs = Math.min(retryDelayMs * 2, RETRY_MAX_MS);
  }

  function refreshList() {
    if (refreshing) return Promise.resolve();
    refreshing = true;
    refreshBtn.classList.add('is-spinning');

    // Without a deadline, a request that never settles (dropped wifi, server
    // restarting) leaves `refreshing` stuck true. The guard above then rejects
    // every later refresh for the life of the page and the button spins forever,
    // with a reload the only way out.
    var ctl = new AbortController();
    var expiry = setTimeout(function () { ctl.abort(); }, REFRESH_TIMEOUT_MS);
    var ok = false;

    return fetch(location.href, { cache: 'no-store', headers: { Accept: 'text/html' }, signal: ctl.signal })
      .then(function (res) {
        // A 401 login page or the /auth/twitch bounce means the session or the
        // Twitch token needs attention — hand the whole page over to the server.
        if (!res.ok || res.redirected) { ok = true; location.reload(); return null; }
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

        listFreshAt = Date.now();
        ok = true;
        renderUptimes();
        applyFilter();
        return probeRooms();
      })
      .catch(function () { /* keep the list we already have */ })
      .then(function () {
        clearTimeout(expiry);
        refreshing = false;
        refreshBtn.classList.remove('is-spinning');
        if (ok) {
          retryDelayMs = RETRY_BASE_MS;
          if (retryTimer) { clearTimeout(retryTimer); retryTimer = null; }
        } else {
          scheduleRetry();
        }
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
    // A unique hit is worth casting wherever it came from — the followed list
    // or the Twitch results.
    var visible = visibleLocalCards().concat(searchCards());
    var typed = finderInput.value.trim();
    var exact = visible.filter(function (c) { return c.dataset.login === typed.toLowerCase(); })[0];
    if (exact) castChannel(exact.dataset.login, exact);
    else if (visible.length === 1) castChannel(visible[0].dataset.login, visible[0]);
    else if (!manualEl.hidden) castChannel(typed.toLowerCase(), null);
  });

  // Coming back to the app has to re-check the world. This used to be gated on
  // hiddenAt, stamped when the page went hidden — but an app frozen or restored
  // without dispatching that event arrives with hiddenAt still 0, so the gate
  // fell through to probeRooms(), which only syncs cast state and never refetches
  // the channel list. Keying off when the list was last known fresh drops the
  // dependency on having caught the outbound event at all.
  function resume() {
    if (document.hidden) return;
    if (Date.now() - listFreshAt > STALE_MS) { refreshList(); return; }
    if (Date.now() - lastProbeAt > PROBE_MIN_GAP_MS) probeRooms();
  }

  document.addEventListener('visibilitychange', resume);
  // visibilitychange alone isn't enough: an installed PWA restored from the
  // back/forward cache can come back without firing it. pageshow does fire on
  // that path, and focus covers switching windows on the desktop, where the page
  // never became hidden in the first place.
  window.addEventListener('pageshow', resume);
  window.addEventListener('focus', resume);
  window.addEventListener('online', resume);
  // Page Lifecycle: a tab unfrozen without a visibility change (frozen while
  // visible) fires only this. Costs nothing to cover.
  document.addEventListener('resume', resume);

  restoreRoom();
  renderUptimes();
  probeRooms();
  // With nobody live, casting by name is the only thing left to do — so put the
  // field in front of the user rather than behind a button.
  if (!localCards().length) toggleFinder(true);
  setInterval(renderUptimes, 60000);
})();

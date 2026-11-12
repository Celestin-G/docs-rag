// Review page client. All database text (question, team reply) goes in with
// textContent. The only innerHTML is draft_html, which the server rendered
// from markdown and sanitised (raw HTML dropped, links restricted).
(function () {
  'use strict';

  var REVIEWER_RE = /^[A-Za-z0-9_-]{8,64}$/;
  var SWIPE_MIN = 70;        // px for a touch swipe
  var WHEEL_MIN = 160;       // accumulated wheel delta for a trackpad swipe
  var WHEEL_QUIET_MS = 350;  // silence that ends a trackpad gesture (and its inertia)

  var $ = function (id) { return document.getElementById(id); };
  var el = {
    progress: $('progress'), undo: $('undo'), loading: $('loading'),
    card: $('card'), question: $('question'), draft: $('draft'),
    replyBox: $('reply-box'), reply: $('reply'), note: $('note'),
    done: $('done'), status: $('status')
  };
  var buttons = Array.prototype.slice.call(document.querySelectorAll('.grade'));

  function randomId() {
    try {
      if (window.crypto && crypto.randomUUID) return crypto.randomUUID();
      if (window.crypto && crypto.getRandomValues) {
        var a = new Uint8Array(16);
        crypto.getRandomValues(a);
        return Array.prototype.map.call(a, function (b) { return ('0' + b.toString(16)).slice(-2); }).join('');
      }
    } catch (e) { /* fall through */ }
    var s = '';
    while (s.length < 32) s += Math.random().toString(16).slice(2);
    return s.slice(0, 32);
  }

  // Anonymous id kept in localStorage; in-memory when storage is unavailable.
  function reviewerId() {
    var id = '';
    try { id = localStorage.getItem('review-reviewer') || ''; } catch (e) { /* ignore */ }
    if (!REVIEWER_RE.test(id)) {
      id = randomId();
      try { localStorage.setItem('review-reviewer', id); } catch (e) { /* ignore */ }
    }
    return id;
  }

  var reviewer = reviewerId();
  var current = null;   // item on screen
  var lastGraded = null; // item_id of the last grade, for "annuler"
  var busy = false;

  function setStatus(msg) { el.status.textContent = msg || ''; }

  function setBusy(b) {
    busy = b;
    buttons.forEach(function (btn) { btn.disabled = b; });
    el.undo.disabled = b;
  }

  function request(method, url, body) {
    var opts = { method: method, headers: {} };
    if (body) {
      opts.headers['Content-Type'] = 'application/json';
      opts.body = JSON.stringify(body);
    }
    return fetch(url, opts).then(function (res) {
      if (res.status === 204) return null;
      return res.text().then(function (txt) {
        var data = null;
        try { data = txt ? JSON.parse(txt) : null; } catch (e) { /* not JSON */ }
        if (!res.ok) {
          var msg = (data && data.error) || (res.status === 429 ? 'Trop de requêtes, patientez un instant.' : 'Erreur ' + res.status);
          throw new Error(msg);
        }
        return data;
      });
    });
  }

  function show(view) {
    el.loading.hidden = true;
    el.progress.textContent = view.progress.graded + ' / ' + view.progress.total;
    el.undo.hidden = lastGraded === null;
    el.note.value = '';
    if (view.done || !view.item) {
      current = null;
      el.card.hidden = true;
      el.done.hidden = false;
      return;
    }
    current = view.item;
    el.done.hidden = true;
    el.question.textContent = view.item.question;
    el.draft.innerHTML = view.item.draft_html; // server-sanitised markdown
    el.reply.textContent = view.item.team_reply;
    el.replyBox.hidden = !view.item.team_reply;
    el.replyBox.open = false;
    el.card.hidden = false;
    window.scrollTo(0, 0);
  }

  function loadNext() {
    return request('GET', '/api/review/next?reviewer=' + encodeURIComponent(reviewer)).then(show);
  }

  function grade(g) {
    if (busy || !current) return;
    var item = current;
    setStatus('');
    setBusy(true);
    request('POST', '/api/review/grades', {
      item_id: item.item_id, reviewer: reviewer, grade: g, note: el.note.value
    }).then(function () {
      lastGraded = item.item_id;
      return loadNext();
    }).catch(function (e) { setStatus(e.message); })
      .then(function () { setBusy(false); });
  }

  function undo() {
    if (busy || lastGraded === null) return;
    var id = lastGraded;
    setStatus('');
    setBusy(true);
    request('DELETE', '/api/review/grades', { item_id: id, reviewer: reviewer })
      .then(function (view) {
        lastGraded = null;
        show(view);
      }).catch(function (e) {
        if (/no grade/.test(e.message)) lastGraded = null;
        setStatus(e.message);
      }).then(function () { setBusy(false); });
  }

  buttons.forEach(function (btn) {
    btn.addEventListener('click', function () { grade(btn.getAttribute('data-grade')); });
  });
  el.undo.addEventListener('click', undo);

  // Keyboard: left bad, up meh, right good; ignored while typing.
  var KEYS = { ArrowLeft: 'bad', ArrowUp: 'meh', ArrowRight: 'good' };
  document.addEventListener('keydown', function (e) {
    var g = KEYS[e.key];
    if (!g || e.repeat || e.altKey || e.ctrlKey || e.metaKey || e.shiftKey) return;
    var t = e.target;
    if (t && (t.tagName === 'TEXTAREA' || t.tagName === 'INPUT' || t.isContentEditable)) return;
    if (!current) return;
    e.preventDefault();
    grade(g);
  });

  // Touch swipe: left bad, right good, up meh. Passive listeners, so native
  // scrolling is untouched; an upward swipe only counts when the page did not
  // scroll during it (short card, or already at the bottom).
  var touch = null;
  function interactive(t) { return t && t.closest && t.closest('textarea, button, a, summary'); }
  document.addEventListener('touchstart', function (e) {
    if (e.touches.length !== 1 || interactive(e.target)) { touch = null; return; }
    var p = e.touches[0];
    touch = { x: p.clientX, y: p.clientY, scroll: window.scrollY };
  }, { passive: true });
  document.addEventListener('touchend', function (e) {
    if (!touch || !e.changedTouches.length) return;
    var p = e.changedTouches[0];
    var dx = p.clientX - touch.x, dy = p.clientY - touch.y;
    var scrolled = Math.abs(window.scrollY - touch.scroll) > 4;
    touch = null;
    if (Math.abs(dx) >= SWIPE_MIN && Math.abs(dx) > Math.abs(dy) * 1.5) {
      grade(dx > 0 ? 'good' : 'bad');
    } else if (dy <= -SWIPE_MIN && Math.abs(dy) > Math.abs(dx) * 1.5 && !scrolled) {
      grade('meh');
    }
  }, { passive: true });
  document.addEventListener('touchcancel', function () { touch = null; }, { passive: true });

  // Trackpad: accumulate wheel deltas into one gesture. After it fires, stay
  // locked until WHEEL_QUIET_MS without events so the inertia tail cannot
  // grade the next card. Direction assumes "natural" scrolling (swiping right
  // gives negative deltaX). Vertical swipes only count when the gesture began at the
  // bottom of the page so ordinary scrolling of a long draft is not hijacked.
  var wheel = { x: 0, y: 0, last: 0, locked: false, startBottom: false };
  function atPageBottom() { return window.innerHeight + window.scrollY >= document.documentElement.scrollHeight - 2; }
  document.addEventListener('wheel', function (e) {
    if (e.ctrlKey || !current || interactive(e.target)) return;
    var now = Date.now();
    if (now - wheel.last > WHEEL_QUIET_MS) {
      wheel.x = 0; wheel.y = 0; wheel.locked = false;
      wheel.startBottom = atPageBottom(); // a gesture that began mid-scroll never counts as a swipe up
    }
    wheel.last = now;
    if (wheel.locked) return;
    wheel.x += e.deltaX;
    wheel.y += e.deltaY;
    var g = null;
    if (Math.abs(wheel.x) >= WHEEL_MIN && Math.abs(wheel.x) > Math.abs(wheel.y) * 1.5) {
      g = wheel.x < 0 ? 'good' : 'bad';
    } else if (wheel.y >= WHEEL_MIN && wheel.y > Math.abs(wheel.x) * 1.5 && wheel.startBottom) {
      g = 'meh';
    }
    if (g) {
      wheel.locked = true;
      wheel.x = 0;
      wheel.y = 0;
      grade(g);
    }
  }, { passive: true });

  loadNext().catch(function (e) {
    el.loading.hidden = true;
    setStatus(e.message);
  });
})();

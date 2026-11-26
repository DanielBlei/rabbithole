// The first-run popup and the guided tour (templates/partials/tour.html). The
// popup offers the tour; the tour dims the page except the control to explain
// (a cut-out in one fixed svg, visual only, so the page stays fully usable) and
// anchors a card beside it. Targets inside the ingestion runner get a plain
// outline instead (the runner's own backdrop already dims the page) and the card
// sits outside the runner frame. The runner keeps one fixed top for the whole
// tour (style.css), so nothing moves under the card. Where the tour is lives in
// sessionStorage so it follows the person across page loads; a state written for
// a database that has since been reset is dropped. A step has one or more
// sections (variants) and each section lists rows; the card is re-evaluated
// whenever the page changes under it.
//
// Sections: data-tour-n is the step number (the total is the largest). A
// section's data-tour-when is a space-separated list of conditions and the
// section applies when any holds; sections are tried in document order and one
// without data-tour-when is the default. Conditions are fixed in holds() below.
//
// Rows (the first row of the chosen section whose path and ring exist wins; with
// no such row the card stays hidden): data-path (regex on the path), data-ring
// (the control to spotlight or outline; a row without one shows the card alone),
// data-side (the side of the target the card tries first), data-to (the step
// Next moves to; Next is the section's data-tour-next button), data-via (the
// control Next clicks to get there: the step is saved and the card hidden first,
// and nothing renders until the new page has loaded; without it the step changes
// in place). A data-tour-run button posts the demo run, data-tour-finish and
// data-tour-end end the tour, data-tour-primary marks the button Arrow Right
// presses. data-ring-also (a selector) adds every visible match to the
// spotlight as one extra hole around them all; a missing match does not block
// the row, and the card anchors to data-ring only. data-point (a selector)
// aims the card's arrow at the first visible match instead of the ring.
//
// Section attributes: data-tour-reveal is a space-separated list of things
// held open while the section shows: topnav (the topbar menus) and rail (the
// side menu). data-tour-advance is a list of conditions; when one holds, the
// step moves on to the section's first row's data-to without a click.
(function(){
  var KEY = 'rh.tour';
  var FIRST_STEP = 'feed';
  var RUNNER_STEPS = ['explain', 'demo'];
  var FEED_PATH = '^/(feed/?)?$';
  var DEMO_RUN_URL = '/ingest/run?scorer=demo';
  var TRACK_MS = 700;       // how long the spotlight follows a moving target
  var SETTLE_MS = 350;      // a swap or fade is judged again after this
  var OPEN_DELAY_MS = 250;  // the page has settled this long before the runner is opened
  var OPEN_TRIES = 30;      // attempts at opening the runner, one per OPEN_DELAY_MS
  var NAV_WAIT_MS = 2000;   // a click that has not left the page by now was held back
  var BOTTOM_RESERVE = 24;  // the card stays this far above the bottom edge
  var PAD = 6;              // spotlight padding around the target
  var OFFSET = 10;          // gap between the card and what it points at
  var MARGIN = 12;          // the card's distance from the viewport edges and the topbar
  var ARROW = 9;            // arrow size, matched in style.css
  var card = document.getElementById('tourCard');
  var dialog = document.getElementById('welcomeModal');
  var mask = document.getElementById('tourMask');
  var root = document.documentElement;
  if (!card && !dialog) return;

  // Resolves true only on the 204 the server answers a recorded step with:
  // fetch rejects on network failure but not on a 500, so the status is read
  // rather than trusted. keepalive lets the write finish across a navigation.
  function record(step){
    return fetch('/welcome/' + step, {method: 'POST', keepalive: true}).then(function(r){
      return r.status === 204;
    }).catch(function(){
      return false;
    });
  }

  // ---- popup -------------------------------------------------------------
  // A stored position is dropped when the page says this is a clean install and
  // the position had already been recorded as started: the database was reset.
  var saved = card ? load() : null;
  if (saved && saved.sync === true && card.hasAttribute('data-tour-fresh')) {
    try { sessionStorage.removeItem(KEY); } catch (err) { /* nothing to clear */ }
    saved = null;
  }
  // A tour that is running or has run in this tab retires the popup for good.
  if (dialog && saved) {
    dialog.hidden = true;
    record('dismissed').then(function(ok){
      if (ok && state) { state.sync = true; save(); }
    });
  } else if (dialog) {
    dialog.hidden = false;
    var writing = false;
    // The x, the backdrop, Escape and both buttons close through modal.js. The
    // dismissal is held until the server confirms it, so a failed write cannot
    // close the popup and bring it back on the next page. The tour starts only
    // after that close, and only when the click came from the tour button.
    document.addEventListener('modal:beforeclose', function(e){
      if (e.detail.layer !== dialog) return;
      e.preventDefault();
      if (writing) return;
      writing = true;
      var go = dialog.getAttribute('data-intent') === 'tour';
      record('dismissed').then(function(ok){
        writing = false;
        if (!ok) {
          if (window.rhToast) window.rhToast.show("Couldn't save that. Try again.", {kind: 'err'});
          return;
        }
        e.detail.proceed();
        if (go) start(true);
      });
    });
    document.addEventListener('keydown', function(){ dialog.removeAttribute('data-intent'); }, true);
    // y and n answer the prompt while the popup is open, and Enter takes the
    // tour unless it is activating a control.
    document.addEventListener('keydown', function(e){
      if (dialog.hidden || e.defaultPrevented || e.altKey || e.ctrlKey || e.metaKey || e.shiftKey) return;
      var key = e.key && e.key.toLowerCase();
      if (key !== 'y' && key !== 'n' && key !== 'enter') return;
      var t = e.target;
      if (t && t.closest && t.closest(key === 'enter' ? 'button, a, summary, input, textarea, select, [contenteditable]' : 'input, textarea, select, [contenteditable]')) return;
      var button = dialog.querySelector(key === 'n' ? '.welcome__acts button:not([data-tour-start])' : '[data-tour-start]');
      if (button) { e.preventDefault(); button.click(); }
    });
  }
  if (!card || !mask) return;

  // ---- state -------------------------------------------------------------
  var state = null;        // {step, sync, open, ran}
  var cur = null;          // {row, ring} of the section on show
  var ringEl = null;
  var alsoEls = [];        // extra spotlight targets of the row on show
  var revealed = {};       // reveal tokens currently applied
  var frame = 0;
  var late = 0;            // timer for the second look after a change
  var placedFor = null;    // target the card was last placed for
  var trackUntil = 0;
  var trackFrame = 0;
  var shown = null;        // section on screen
  var origin = null;       // the control that started the tour, for returning focus
  var wantFocus = false;   // the card takes focus the next time it shows
  var navigating = false;  // a click is carrying the page away; nothing renders meanwhile
  var total = 0;
  var dim = mask.querySelector('.tour-mask__dim');
  var edge = mask.querySelector('.tour-mask__edge');
  Array.prototype.forEach.call(card.querySelectorAll('[data-tour-n]'), function(s){
    total = Math.max(total, parseInt(s.getAttribute('data-tour-n'), 10) || 0);
  });
  var SIDES = ['tour--below', 'tour--above', 'tour--left', 'tour--right'];
  card.setAttribute('tabindex', '-1');
  if (window.htmx) htmx.process(card);

  function load(){
    try {
      var s = JSON.parse(sessionStorage.getItem(KEY));
      return s && typeof s.step === 'string' ? s : null;
    } catch (err) {
      return null;
    }
  }
  function save(){
    try {
      sessionStorage.setItem(KEY, JSON.stringify(state));
      return true;
    } catch (err) {
      return false;
    }
  }
  function matches(source, text){
    try { return new RegExp(source).test(text); } catch (err) { return false; }
  }

  // ---- visibility --------------------------------------------------------
  // A control counts when it has a box and is not faded out; a closed layer
  // keeps no content, and a faded ancestor hides what it holds.
  function visible(el){
    if (!el.getClientRects().length) return false;
    var r = el.getBoundingClientRect();
    if (!(r.width > 0 && r.height > 0)) return false;
    var layer = el.closest('#modal, #modalTop');
    if (layer && !layer.firstElementChild) return false;
    var opacity = 1;
    for (var n = el; n && n.nodeType === 1; n = n.parentElement) {
      var cs = getComputedStyle(n);
      if (cs.display === 'none' || cs.visibility === 'hidden') return false;
      opacity *= parseFloat(cs.opacity);
      if (!(opacity > 0)) return false;
    }
    return true;
  }
  function firstVisible(selector){
    var list;
    try { list = document.querySelectorAll(selector); } catch (err) { return null; }
    return Array.prototype.filter.call(list, visible)[0] || null;
  }
  function inRunner(el){
    return !!el.closest('#modal, #modalTop');
  }
  function runnerFrame(){
    return firstVisible('#modal .modal__frame--ingest');
  }

  // ---- choosing what to show ---------------------------------------------
  function sectionsOf(step){
    return Array.prototype.filter.call(card.querySelectorAll('[data-tour-step]'), function(s){
      return s.getAttribute('data-tour-step') === step;
    });
  }
  function holds(token){
    switch (token) {
      case 'runner-open': return !!runnerFrame();
      case 'at-sources': return matches('^/sources', location.pathname);
      case 'run-failed': return !!state.ran && !!firstVisible('#modal .ing__errline');
      case 'demo-done': return !!firstVisible('#modal .ing__done[data-demo]');
      case 'running': return !!firstVisible('#modal .ing__st--run');
      case 'demo-row': return !!firstVisible('.row[data-demo]');
    }
    return false;
  }
  function choose(list){
    for (var i = 0; i < list.length; i++) {
      var when = list[i].getAttribute('data-tour-when');
      if (!when || when.split(/\s+/).some(holds)) return list[i];
    }
    return null;
  }
  // The runner steps wait for the runner; a step that is not on its page waits.
  // The Feed's own rows settle with the page, so the card waits for that too.
  function pick(section){
    if (section.getAttribute('data-tour-step') === 'done' && document.readyState !== 'complete') return null;
    var rows = section.querySelectorAll('[data-tour-on]');
    for (var i = 0; i < rows.length; i++) {
      var row = rows[i];
      if (!matches(row.getAttribute('data-path') || '.*', location.pathname)) continue;
      var ringSel = row.getAttribute('data-ring');
      var ring = null;
      if (ringSel) {
        ring = firstVisible(ringSel);
        if (!ring) continue;
      }
      var alsoSel = row.getAttribute('data-ring-also');
      var also = [];
      if (alsoSel) {
        try { also = Array.prototype.filter.call(document.querySelectorAll(alsoSel), visible); } catch (err) { also = []; }
      }
      return {row: row, ring: ring, also: also};
    }
    return null;
  }

  // ---- reveal ------------------------------------------------------------
  // Menus the section points at are held open while it shows and let go after.
  function applyReveal(tokens){
    var want = {};
    tokens.forEach(function(t){ want[t] = true; });
    Object.keys(revealed).forEach(function(t){
      if (want[t]) return;
      setReveal(t, false);
      delete revealed[t];
    });
    Object.keys(want).forEach(function(t){
      if (revealed[t]) return;
      setReveal(t, true);
      revealed[t] = true;
    });
  }
  function setReveal(token, on){
    if (token === 'topnav') {
      Array.prototype.forEach.call(document.querySelectorAll('.topbar__nav .navgrp'), function(g){
        g.classList.toggle('tour-open', on);
      });
    } else if (token === 'rail') {
      if (window.navrail && window.navrail.hold) window.navrail.hold(on);
    }
  }
  function tokensOf(section, attr){
    var v = section && section.getAttribute(attr);
    return v ? v.split(/\s+/).filter(Boolean) : [];
  }

  // ---- target ------------------------------------------------------------
  // Only a target in the runner is outlined; the page gets the spotlight.
  function setRing(el){
    if (ringEl && ringEl !== el) ringEl.classList.remove('tour-ring');
    if (el && inRunner(el)) el.classList.add('tour-ring');
    ringEl = el;
  }

  // ---- render ------------------------------------------------------------
  function hideCard(){
    cur = null;
    shown = null;
    alsoEls = [];
    setRing(null);
    card.hidden = true;
    drawMask(null);
  }
  // A section whose advance condition holds moves the step on by itself.
  function advance(list){
    for (var i = 0; i < list.length; i++) {
      if (!tokensOf(list[i], 'data-tour-advance').some(holds)) continue;
      var row = list[i].querySelector('[data-tour-on]');
      var to = row && row.getAttribute('data-to');
      if (!to || to === state.step) continue;
      state.step = to;
      return save() ? true : (end(), false);
    }
    return false;
  }
  function render(depth){
    if (!state || navigating) return;
    var list = sectionsOf(state.step);
    if (!list.length) { end(); return; }
    if (!(depth > 3) && advance(list)) { render((depth || 0) + 1); return; }
    if (!state) return;
    var section = choose(list);
    applyReveal(tokensOf(section, 'data-tour-reveal'));
    var picked = section && pick(section);
    if (!picked) {
      Array.prototype.forEach.call(card.querySelectorAll('[data-tour-step]'), function(s){ s.hidden = true; });
      hideCard();
      return;
    }
    var had = card.contains(document.activeElement);
    Array.prototype.forEach.call(card.querySelectorAll('[data-tour-step]'), function(s){
      s.hidden = s !== section;
    });
    var n = section.getAttribute('data-tour-n');
    section.querySelector('[data-tour-count]').textContent = n + ' of ' + total;
    card.setAttribute('aria-label', 'Guided tour, step ' + n + ' of ' + total);
    cur = picked;
    alsoEls = picked.also;
    setRing(cur.ring);
    card.hidden = false;
    var moved = shown !== section;
    var stepMoved = shown !== null && shown.getAttribute('data-tour-step') !== section.getAttribute('data-tour-step');
    shown = section;
    if (ringEl !== placedFor) {
      placedFor = ringEl;
      if (ringEl) scrollToRing();
      track(TRACK_MS);
    }
    place();
    // Focus follows the card when its step changes, or when the button that
    // held it has been swapped for the next section's.
    if (wantFocus || stepMoved || (moved && had)) {
      wantFocus = false;
      focusPrimary();
    }
  }
  function topEdge(){
    var bar = document.querySelector('.topbar');
    return Math.max(bar ? bar.getBoundingClientRect().bottom : 0, 0);
  }
  function scrollToRing(){
    var r = ringEl.getBoundingClientRect();
    if (inRunner(ringEl)) {
      ringEl.scrollIntoView({block: 'nearest', behavior: 'instant'});
    } else if (r.width > 0 && (r.top < topEdge() || r.bottom > innerHeight)) {
      ringEl.scrollIntoView({block: 'center', behavior: 'instant'});
    }
  }
  // The section's primary button, else the card itself.
  function focusPrimary(){
    if (card.hidden) return;
    var list = card.querySelectorAll('[data-tour-primary]');
    for (var i = 0; i < list.length; i++) {
      if (list[i].getClientRects().length) { list[i].focus(); return; }
    }
    card.focus();
  }

  // ---- spotlight ---------------------------------------------------------
  function radius(el){
    var cs = getComputedStyle(el).borderTopLeftRadius;
    var v = parseFloat(cs) || 0;
    return /%/.test(cs) ? v / 100 * Math.min(el.offsetWidth, el.offsetHeight) : v;
  }
  function holePath(r, el){
    var x = r.left - PAD, y = r.top - PAD, hw = r.width + 2 * PAD, hh = r.height + 2 * PAD;
    var k = Math.min(radius(el) + PAD, hw / 2, hh / 2);
    return 'M' + (x + k) + ' ' + y + 'H' + (x + hw - k) + 'A' + k + ' ' + k + ' 0 0 1 ' + (x + hw) + ' ' + (y + k) +
      'V' + (y + hh - k) + 'A' + k + ' ' + k + ' 0 0 1 ' + (x + hw - k) + ' ' + (y + hh) +
      'H' + (x + k) + 'A' + k + ' ' + k + ' 0 0 1 ' + x + ' ' + (y + hh - k) +
      'V' + (y + k) + 'A' + k + ' ' + k + ' 0 0 1 ' + (x + k) + ' ' + y + 'Z';
  }
  // The box around every extra target, with the first one's corner radius.
  function alsoHole(){
    var els = alsoEls.filter(function(e){ return e.isConnected; });
    var box = null;
    els.forEach(function(e){
      var b = e.getBoundingClientRect();
      if (!(b.width > 0 && b.height > 0)) return;
      box = box ? {left: Math.min(box.left, b.left), top: Math.min(box.top, b.top), right: Math.max(box.right, b.right), bottom: Math.max(box.bottom, b.bottom)} :
        {left: b.left, top: b.top, right: b.right, bottom: b.bottom};
    });
    if (!box) return '';
    box.width = box.right - box.left;
    box.height = box.bottom - box.top;
    return holePath(box, els[0]);
  }
  // One path: the viewport rectangle and rounded holes, filled even-odd; the
  // edge is a 1px accent outline around the holes.
  function drawMask(r){
    mask.classList.toggle('is-on', !!r);
    if (!r) return;
    var holes = holePath(r, ringEl) + alsoHole();
    dim.setAttribute('d', 'M0 0H' + viewWidth() + 'V' + innerHeight + 'H0Z' + holes);
    edge.setAttribute('d', holes);
  }

  // ---- placement ---------------------------------------------------------
  // The page's own width: the scrollbar's strip is not part of it.
  function viewWidth(){ return root.getBoundingClientRect().width; }
  function clamp(v, lo, hi){ return Math.max(lo, Math.min(v, hi)); }
  function clearPlacement(){
    SIDES.forEach(function(c){ card.classList.remove(c); });
    card.classList.remove('tour--modal');
  }
  // Puts the card where it never covers what it explains and always keeps its
  // width: below, above, right or left of the box it points at (the row may name
  // the side to try first), start-aligned and shifted into the viewport, arrow on
  // the target's centre, between the topbar and BOTTOM_RESERVE above the bottom
  // edge. Beside a runner target the box is the runner frame, and the card tries
  // the sides first (style.css makes room for it). With no spot that fits, or no
  // target, it sits centred under the topbar.
  function place(){
    if (!state || card.hidden) return;
    clearPlacement();
    var vw = viewWidth(), vh = innerHeight;
    var top0 = topEdge(), bottom = vh - BOTTOM_RESERVE;
    var r = ringEl && ringEl.isConnected ? ringEl.getBoundingClientRect() : null;
    if (r && !(r.width > 0 && r.height > 0)) r = null;
    var runner = ringEl && inRunner(ringEl) ? runnerFrame() : null;
    var box = null, order;
    if (runner) {
      card.classList.add('tour--modal');
      drawMask(null);
      var f = runner.getBoundingClientRect();
      box = {left: f.left, top: f.top, right: f.right, bottom: f.bottom};
      order = ['right', 'left', 'below', 'above'];
    } else if (r) {
      drawMask(r);
      box = {left: r.left - PAD, top: r.top - PAD, right: r.right + PAD, bottom: r.bottom + PAD};
      order = ['below', 'above', 'right', 'left'];
      var first = cur && cur.row.getAttribute('data-side');
      if (first) order = [first].concat(order.filter(function(s){ return s !== first; }));
    } else {
      drawMask(null);
    }
    var cw = card.offsetWidth, ch = card.offsetHeight;
    var side = null, left = 0, top = 0;
    if (box) {
      var spot = {
        below: function(){ return [clamp(box.left, MARGIN, vw - MARGIN - cw), box.bottom + OFFSET]; },
        above: function(){ return [clamp(box.left, MARGIN, vw - MARGIN - cw), box.top - OFFSET - ch]; },
        right: function(){ return [box.right + OFFSET, clamp(r ? r.top + r.height / 2 - ch / 2 : box.top, top0, bottom - ch)]; },
        left: function(){ return [box.left - OFFSET - cw, clamp(r ? r.top + r.height / 2 - ch / 2 : box.top, top0, bottom - ch)]; }
      };
      for (var i = 0; i < order.length && !side; i++) {
        var p = spot[order[i]]();
        // Below its target a card is under the topbar whatever the target is.
        if (p[0] < MARGIN || p[0] + cw > vw - MARGIN || (p[1] < top0 && order[i] !== 'below') || p[1] + ch > bottom) continue;
        side = order[i]; left = p[0]; top = p[1];
      }
    }
    if (side && r) {
      var horizontal = side === 'below' || side === 'above';
      var pointSel = cur && cur.row.getAttribute('data-point');
      var pointEl = pointSel ? firstVisible(pointSel) : null;
      var pr = pointEl ? pointEl.getBoundingClientRect() : r;
      var pos = horizontal ? pr.left + pr.width / 2 - left : pr.top + pr.height / 2 - top;
      card.style.setProperty('--tour-arrow', clamp(pos, 2 * ARROW, (horizontal ? cw : ch) - 2 * ARROW) + 'px');
      card.classList.add('tour--' + side);
    } else if (!side) {
      left = clamp((vw - cw) / 2, MARGIN, Math.max(MARGIN, vw - MARGIN - cw));
      top = top0 + MARGIN;
    }
    card.style.left = Math.round(left) + 'px';
    card.style.top = Math.round(top) + 'px';
  }
  // The runner opens and swaps settle, so the spotlight keeps following its
  // target for a while.
  function track(ms){
    trackUntil = Math.max(trackUntil, Date.now() + ms);
    if (trackFrame) return;
    var tick = function(){
      trackFrame = 0;
      place();
      if (state && Date.now() < trackUntil) trackFrame = requestAnimationFrame(tick);
    };
    trackFrame = requestAnimationFrame(tick);
  }
  function pass(){
    frame = 0;
    if (state && !navigating) render();
  }
  function schedule(){
    if (!frame) frame = requestAnimationFrame(pass);
  }

  // Changes the tour itself makes (the card, the mask and the outline) are not
  // the page changing under it.
  function own(rec){
    var t = rec.target;
    if (card.contains(t) || t === mask) return true;
    if (rec.type !== 'attributes' || rec.attributeName !== 'class') return false;
    // Holding the menus open flips classes on these.
    return t === ringEl || t === document.body || t.id === 'navRail' || (t.classList && t.classList.contains('navgrp'));
  }
  var watcher = new MutationObserver(function(records){
    if (!state || navigating || records.every(own)) return;
    schedule();
    // A fading layer or a settling swap is judged once more after it lands.
    clearTimeout(late);
    late = setTimeout(schedule, SETTLE_MS);
  });
  function activate(){
    root.classList.add('tour-on');
    watcher.observe(document.body, {childList: true, subtree: true, attributes: true, attributeFilter: ['class', 'hidden', 'style']});
  }

  // Ending by the person's own action hands focus back to what started the
  // tour, or an open runner, or the page.
  function giveBack(){
    var open = document.querySelector('#modalTop .modal__frame') || document.querySelector('#modal .modal__frame');
    var el = open || (origin && origin.isConnected && origin.getClientRects().length ? origin : document.getElementById('main'));
    origin = null;
    if (el) el.focus();
  }
  function end(byPerson){
    state = null;
    navigating = false;
    clearTimeout(late);
    try { sessionStorage.removeItem(KEY); } catch (err) { /* nothing to clear */ }
    hideCard();
    applyReveal([]);
    watcher.disconnect();
    root.classList.remove('tour-on');
    placedFor = null;
    clearPlacement();
    if (byPerson) giveBack();
  }

  // ---- moving on ---------------------------------------------------------
  // A click that leaves the page hides the card first and saves the next step,
  // so the old page never shows the new step's counter over the old copy. If
  // the click was held back (a leave guard), the step it came from returns.
  function leave(via, to, from){
    var link = firstVisible(via) || document.querySelector(via);
    if (!link) return false;
    navigating = true;
    hideCard();
    applyReveal([]);
    state.step = to;
    state.open = to === RUNNER_STEPS[0];
    if (!save()) { end(); return true; }
    link.click();
    setTimeout(function(){
      if (!state || !navigating) return;
      navigating = false;
      state.step = from;
      state.open = false;
      save();
      render();
    }, NAV_WAIT_MS);
    return true;
  }
  function next(){
    var row = cur && cur.row;
    var to = row && row.getAttribute('data-to');
    if (!to) return;
    var via = row.getAttribute('data-via');
    if (via) { leave(via, to, state.step); return; }
    state.step = to;
    if (to === RUNNER_STEPS[0] && !runnerFrame()) state.open = true;
    if (!save()) { end(); return; }
    if (state.open) openRunner(0);
    render();
  }
  // The runner is opened for the person once the page has settled; only a Next
  // that navigated asks for it, so a runner they close stays closed.
  function openRunner(tries){
    var chip = document.getElementById('ingChip');
    if (!chip || !visible(chip)) chip = document.getElementById('navIngest');
    if (runnerFrame()) { state.open = false; save(); return; }
    if (document.readyState === 'complete' && window.htmx && chip && visible(chip)) {
      state.open = false;
      save();
      chip.click();
      return;
    }
    if (tries < OPEN_TRIES) setTimeout(function(){ if (state) openRunner(tries + 1); }, OPEN_DELAY_MS);
    else { state.open = false; save(); }
  }
  // The demo run is the only run the tour starts, so the runner's own Run
  // controls are redirected to it while the runner is being explained.
  function runDemo(){
    state.step = RUNNER_STEPS[1];
    state.ran = true;
    save();
    if (window.htmx) htmx.ajax('POST', DEMO_RUN_URL, {target: '#ingestBody', swap: 'outerHTML'});
  }

  // ---- start -------------------------------------------------------------
  function start(fromPopup){
    // Starting retires the popup even when it was never answered.
    state = {step: FIRST_STEP};
    if (fromPopup) state.sync = true;
    if (!save()) { state = null; return; }
    shown = null;
    activate();
    if (matches(FEED_PATH, location.pathname)) {
      if (!fromPopup) {
        record('dismissed').then(function(ok){
          if (ok && state) { state.sync = true; save(); }
        });
      }
      wantFocus = true;
      render();
      return;
    }
    // The first step lives on the Feed page: carry the person there first.
    var go = function(){
      wantFocus = true;
      var link = document.querySelector('.topbar__nav a[href="/feed"]');
      navigating = true;
      hideCard();
      if (link) link.click(); else location.assign('/feed');
      setTimeout(function(){
        if (!state || !navigating) return;
        navigating = false;
        render();
      }, NAV_WAIT_MS);
    };
    if (fromPopup) { go(); return; }
    record('dismissed').then(function(ok){
      if (ok && state) { state.sync = true; save(); }
      if (state) go();
    });
  }

  // ---- events ------------------------------------------------------------
  // Capture phase: the state is written before the click reaches the control
  // (and before settings.js can hold it behind its unsaved-changes dialog).
  document.addEventListener('click', function(e){
    var t = e.target;
    if (!t || !t.closest) return;
    var starter = t.closest('[data-tour-start]');
    var inPopup = !!(dialog && starter && dialog.contains(starter));
    if (dialog) {
      if (inPopup) dialog.setAttribute('data-intent', 'tour');
      else dialog.removeAttribute('data-intent');
    }
    if (starter && !inPopup) { origin = starter; start(); return; }
    if (state) {
      if (card.contains(t)) {
        // A button that is busy does nothing, and no handler below it hears it.
        if (t.closest('[aria-disabled="true"]')) { e.preventDefault(); e.stopPropagation(); return; }
        if (t.closest('[data-tour-end]')) { end(true); return; }
        if (t.closest('.tour__link')) { end(); return; }
        if (t.closest('[data-tour-finish]')) { end(true); return; }
        if (t.closest('[data-tour-run]')) { state.step = RUNNER_STEPS[1]; state.ran = true; save(); }
        if (t.closest('[data-tour-next]')) next();
      } else if (RUNNER_STEPS.indexOf(state.step) >= 0) {
        var run = t.closest('#modal [hx-post="/ingest/run"]');
        if (run && !run.disabled) {
          e.preventDefault();
          e.stopPropagation();
          runDemo();
        }
      }
    }
    // Look again once this click has finished.
    setTimeout(pass, 0);
  }, true);
  window.addEventListener('resize', place);
  window.addEventListener('scroll', place, {passive: true, capture: true});
  window.addEventListener('popstate', pass);
  window.addEventListener('load', pass);
  // A page restored from the back-forward cache is no longer mid-navigation.
  window.addEventListener('pageshow', function(e){
    if (e.persisted) { navigating = false; pass(); }
  });
  // Escape and Arrow Right belong to the tour only while focus is in the card;
  // capture phase, so modal.js sees Escape already handled and leaves the runner.
  document.addEventListener('keydown', function(e){
    if (!state || card.hidden || e.altKey || e.ctrlKey || e.metaKey || !card.contains(document.activeElement)) return;
    if (e.key === 'Escape') {
      e.preventDefault();
      end(true);
    } else if (e.key === 'ArrowRight') {
      var primary = card.querySelector('section:not([hidden]) [data-tour-primary]');
      if (primary && primary.getAttribute('aria-disabled') !== 'true') {
        e.preventDefault();
        primary.click();
      }
    }
  }, true);

  // ---- boot --------------------------------------------------------------
  // A tour that was already running arrives on a new page: the card takes focus once.
  state = saved;
  if (state) {
    activate();
    wantFocus = true;
    render();
    if (state.open) openRunner(0);
  }
})();

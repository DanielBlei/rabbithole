// What a request looks like while it is in flight, and what it says when it
// fails. Everything here is delegated on document, so it covers htmx markup
// swapped in later without rebinding.

// Progress bar. Local requests usually land in a few milliseconds, so showing
// it immediately would be a flash on every click; it waits SHOW_AFTER before
// appearing and only then animates. The background poller is exempt — a bar
// blinking every 3s through an ingest run is noise, not feedback.
(function(){
  var SHOW_AFTER = 140;
  var bar = null, showT = null, live = 0;

  function isPoll(elt){
    if (!elt || !elt.getAttribute) return false;
    var trig = elt.getAttribute('hx-trigger') || '';
    return trig.indexOf('every') !== -1;
  }
  function ensure(){
    if (bar) return bar;
    bar = document.createElement('div');
    bar.className = 'netbar';
    bar.setAttribute('aria-hidden', 'true');
    document.body.appendChild(bar);
    return bar;
  }
  function start(){
    if (++live > 1) return;
    clearTimeout(showT);
    showT = setTimeout(function(){ ensure().classList.add('netbar--on'); }, SHOW_AFTER);
  }
  function stop(){
    // Never let a stray afterRequest drive the count negative: the bar would
    // then need two starts before it showed again.
    if (live > 0) live--;
    if (live > 0) return;
    clearTimeout(showT);
    if (bar) bar.classList.remove('netbar--on');
  }

  document.addEventListener('htmx:beforeRequest', function(e){
    if (!isPoll(e.detail.elt)) start();
  });
  document.addEventListener('htmx:afterRequest', function(e){
    if (!isPoll(e.detail.elt)) stop();
  });
})();

// Toasts. Without the failure kind a failed request is a dead click: htmx
// swaps nothing and the page sits there looking like it ignored you. The ok
// kind confirms a change and can carry an Undo.
(function(){
  var LINGER = { err: 6000, ok: 4000, undo: 8000 };
  var HELD_MIN = 1500; // what a toast keeps once the pointer or focus leaves it
  var MAX = 2;
  var wrap = null, shown = {}, order = [];

  function ensure(){
    if (wrap) return wrap;
    wrap = document.createElement('div');
    wrap.className = 'toasts';
    // polite, not assertive: a failed fetch is worth saying, not worth cutting
    // off whatever the screen reader is in the middle of.
    wrap.setAttribute('role', 'status');
    wrap.setAttribute('aria-live', 'polite');
    document.body.appendChild(wrap);
    return wrap;
  }

  function dismiss(entry){
    if (shown[entry.key] !== entry) return;
    clearTimeout(entry.timer);
    delete shown[entry.key];
    order.splice(order.indexOf(entry.key), 1);
    entry.el.classList.remove('toast--on');
    setTimeout(function(){ if (entry.el.parentNode) entry.el.parentNode.removeChild(entry.el); }, 200);
  }

  function arm(entry, ms){
    clearTimeout(entry.timer);
    entry.due = Date.now() + ms;
    entry.timer = setTimeout(function(){ dismiss(entry); }, ms);
  }

  // A toast stays while the pointer is over it or focus is inside it, so an
  // Undo cannot vanish under the cursor.
  function holdOnInteract(entry){
    var held = { pointer: false, focus: false };
    function sync(){
      var hold = held.pointer || held.focus;
      if (hold && !entry.held) {
        entry.held = true;
        clearTimeout(entry.timer);
        entry.rest = Math.max(entry.due - Date.now(), HELD_MIN);
      } else if (!hold && entry.held) {
        entry.held = false;
        arm(entry, entry.rest);
      }
    }
    entry.el.addEventListener('mouseenter', function(){ held.pointer = true; sync(); });
    entry.el.addEventListener('mouseleave', function(){ held.pointer = false; sync(); });
    entry.el.addEventListener('focusin', function(){ held.focus = true; sync(); });
    entry.el.addEventListener('focusout', function(){ held.focus = false; sync(); });
  }

  // Over the cap the oldest plain toast goes first; an Undo is only evicted
  // when every toast on screen has one.
  function evict(){
    var plain = order.filter(function(k){ return !shown[k].undo; });
    dismiss(shown[plain.length ? plain[0] : order[0]]);
  }

  // opts: kind ('ok' or the default failure), undo (a token, which adds the
  // Undo button), ids (the sources it covers, so the Undo can ask first when
  // one has an editor with unsaved changes) and linger (ms).
  function show(msg, opts){
    opts = opts || {};
    var kind = opts.kind === 'ok' ? 'ok' : 'err';
    var undo = opts.undo || '';
    var linger = opts.linger || LINGER[undo ? 'undo' : kind];
    // The same message twice (a retry, or a poller looping) refreshes the one
    // on screen instead of stacking copies of itself. An undo toast is its
    // own action, so it is keyed by token.
    var key = undo ? 'u:' + undo : kind + ':' + msg;
    var open = shown[key];
    if (open) {
      if (open.held) open.rest = linger; else arm(open, linger);
      return;
    }
    while (order.length >= MAX) evict();

    var el = document.createElement('div');
    el.className = 'toast' + (kind === 'ok' ? ' toast--ok' : '');

    var text = document.createElement('span');
    text.className = 'toast__msg';
    text.textContent = msg;
    el.appendChild(text);

    var entry = { key: key, el: el, undo: !!undo };
    arm(entry, linger);
    holdOnInteract(entry);

    if (undo) {
      var act = document.createElement('button');
      act.className = 'toast__act';
      act.type = 'button';
      act.textContent = 'Undo';
      act.addEventListener('click', function(){
        function run(){
          htmx.ajax('POST', '/sources/undo/' + encodeURIComponent(undo), { swap: 'none' });
          dismiss(entry);
        }
        var ids = opts.ids ? opts.ids.split(',') : [];
        if (ids.length && window.rhSourcesGuard) window.rhSourcesGuard(ids, run);
        else run();
      });
      el.appendChild(act);
    }

    var x = document.createElement('button');
    x.className = 'toast__x';
    x.type = 'button';
    x.setAttribute('aria-label', 'Dismiss');
    x.innerHTML = '&times;';
    x.addEventListener('click', function(){ dismiss(entry); });
    el.appendChild(x);

    ensure().appendChild(el);
    shown[key] = entry;
    order.push(key);

    // Next frame, so the element is in the document before the class that
    // transitions it in, otherwise it just appears.
    requestAnimationFrame(function(){ el.classList.add('toast--on'); });
  }

  window.rhToast = { show: show };

  // These routes answer a refusal with a sentence meant for the person.
  var PLAIN_REFUSAL = /^\/sources\/(bulk|export|undo\/)/;

  function forStatus(status){
    if (status === 404) return 'That is no longer there. It may have been removed.';
    if (status === 409) return 'That changed somewhere else. Reopen it and try again.';
    if (status === 410) return 'That can no longer be undone.';
    if (status >= 500) return 'The server failed on that request.';
    if (status >= 400) return 'The server refused that request.';
    return 'That request failed.';
  }

  function refusal(detail){
    var xhr = detail.xhr;
    var path = (detail.pathInfo && detail.pathInfo.requestPath) || '';
    if (xhr.status !== 400 || !PLAIN_REFUSAL.test(path)) return '';
    var type = xhr.getResponseHeader('Content-Type') || '';
    var body = (xhr.responseText || '').trim();
    return type.indexOf('text/plain') === 0 && body && body.length < 200 ? body : '';
  }

  document.addEventListener('htmx:responseError', function(e){
    show(refusal(e.detail) || forStatus(e.detail.xhr.status));
  });
  document.addEventListener('htmx:sendError', function(){
    show('Cannot reach the server. Check that it is still running.');
  });
  document.addEventListener('htmx:timeout', function(){
    show('That request took too long and was given up on.');
  });

  // The server sends #srcFlash as a hidden marker carrying data-msg, and for a
  // change that can be taken back data-undo (its token) and data-ids (the
  // sources it covers). htmx fills the marker's attributes when the swap
  // settles, after its swap events, so this reads whatever is on the page each
  // time the marker or the page changes. Reading empties it first, so a message
  // shows once and a later swap cannot bring it back.
  function flash(){
    var el = document.getElementById('srcFlash');
    var msg = el && el.getAttribute('data-msg');
    if (!msg) return;
    var undo = el.getAttribute('data-undo');
    var ids = el.getAttribute('data-ids');
    el.removeAttribute('data-msg');
    el.removeAttribute('data-undo');
    el.removeAttribute('data-ids');
    el.textContent = '';
    show(msg, { kind: 'ok', undo: undo || undefined, ids: ids || undefined });
  }

  new MutationObserver(flash).observe(document.documentElement, {
    subtree: true, childList: true, attributes: true, attributeFilter: ['data-msg']
  });
  document.addEventListener('htmx:afterSettle', flash);
})();

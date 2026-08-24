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

// Failure toast. Without this a failed request is a dead click: htmx swaps
// nothing and the page sits there looking like it ignored you.
(function(){
  var LINGER = 6000;
  var wrap = null, shown = {};

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

  function dismiss(el, msg){
    el.classList.remove('toast--on');
    delete shown[msg];
    setTimeout(function(){ if (el.parentNode) el.parentNode.removeChild(el); }, 200);
  }

  function toast(msg){
    // Same failure twice (a retry, or a poller looping) refreshes the one on
    // screen instead of stacking copies of itself.
    var open = shown[msg];
    if (open) {
      clearTimeout(open.timer);
      open.timer = setTimeout(function(){ dismiss(open.el, msg); }, LINGER);
      return;
    }
    var el = document.createElement('div');
    el.className = 'toast';

    var text = document.createElement('span');
    text.className = 'toast__msg';
    text.textContent = msg;

    var x = document.createElement('button');
    x.className = 'toast__x';
    x.type = 'button';
    x.setAttribute('aria-label', 'Dismiss');
    x.innerHTML = '&times;';

    el.appendChild(text);
    el.appendChild(x);
    ensure().appendChild(el);

    var entry = { el: el, timer: setTimeout(function(){ dismiss(el, msg); }, LINGER) };
    shown[msg] = entry;
    x.addEventListener('click', function(){ clearTimeout(entry.timer); dismiss(el, msg); });

    // Next frame, so the element is in the document before the class that
    // transitions it in — otherwise it just appears.
    requestAnimationFrame(function(){ el.classList.add('toast--on'); });
  }

  function forStatus(status){
    if (status === 404) return 'That is no longer there. It may have been removed.';
    if (status === 409) return 'That changed somewhere else. Reopen it and try again.';
    if (status >= 500) return 'The server failed on that request.';
    if (status >= 400) return 'The server refused that request.';
    return 'That request failed.';
  }

  document.addEventListener('htmx:responseError', function(e){
    toast(forStatus(e.detail.xhr.status));
  });
  document.addEventListener('htmx:sendError', function(){
    toast('Cannot reach the server. Check that it is still running.');
  });
  document.addEventListener('htmx:timeout', function(){
    toast('That request took too long and was given up on.');
  });
})();

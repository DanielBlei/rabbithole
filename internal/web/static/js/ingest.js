// Ambient ingest wiring: run logs, live clocks, the Ingestion page's rows, and
// the page-wide refresh that fires when a run starts or finishes.

// Run logs (the runner modal and the Ingestion page's run details) follow new
// lines only for a reader who was at the bottom. The reader's place is noted
// just before a swap and put back just after, keyed by the nearest element with
// an id (a log's owner). A log that is replaced (the modal's every poll, a run
// opened, the debug lines fetched) takes its note once, and only while fresh; a
// log with no usable note is new and opens at the bottom. A live page log is
// appended to in place, so it keeps its scroll and selection by itself and only
// needs to follow when it was at the bottom. A focused info/debug pill is noted
// the same way and refocused in its replacement.
(function(){
  var NEAR = 24, FRESH = 1500;
  var notes = {}, pill = null;
  function key(el){
    var o = el.closest('[id]');
    return o ? o.id : '';
  }
  function atBottom(el){
    return el.scrollHeight - el.scrollTop - el.clientHeight <= NEAR;
  }
  function note(){
    var now = performance.now();
    document.querySelectorAll('[data-ing-lines]').forEach(function(el){
      notes[key(el)] = { top: el.scrollTop, bottom: atBottom(el), at: now };
    });
    var f = document.activeElement;
    if (f && f.hasAttribute && f.hasAttribute('data-ing-lvl')){
      pill = { el: f, id: key(f), lvl: f.getAttribute('data-ing-lvl'), at: now };
    }
  }
  function place(){
    var now = performance.now();
    document.querySelectorAll('[data-ing-lines]').forEach(function(el){
      if (el.dataset.placed){
        var cur = notes[key(el)];
        if (cur && now - cur.at <= FRESH && cur.bottom) el.scrollTop = el.scrollHeight;
        return;
      }
      el.dataset.placed = '1';
      var k = key(el), n = notes[k];
      if (n) delete notes[k];
      if (n && now - n.at > FRESH) n = null;
      el.scrollTop = !n || n.bottom ? el.scrollHeight : n.top;
    });
    // The note outlives the response's other swaps (an out-of-band part runs
    // this before the log's own swap) until the pill is refocused in a new
    // element or the note goes stale.
    if (pill && now - pill.at > FRESH) pill = null;
    if (pill){
      var owner = pill.id && document.getElementById(pill.id);
      var btn = owner && owner.querySelector('[data-ing-lvl="' + pill.lvl + '"]');
      if (btn && btn !== pill.el && btn.isConnected){
        btn.focus({ preventScroll: true });
        pill = null;
      }
    }
  }
  document.body.addEventListener('htmx:beforeSwap', note);
  document.body.addEventListener('htmx:oobBeforeSwap', note);
  document.body.addEventListener('htmx:afterSwap', place);
  document.body.addEventListener('htmx:oobAfterSwap', place);
  place();

  // Changing the level reflows every log. One that was at the bottom stays there.
  // A page log opens with info lines only: the first time debug is picked, each
  // open log without them is fetched whole (from its own element, so the
  // requests do not share a slot). Row requests and refreshes carry the picked
  // level (see the configRequest hook below); a live log's cursor carries its own.
  document.addEventListener('click', function(e){
    var btn = e.target.closest('[data-ing-lvl]');
    if (!btn) return;
    var host = btn.closest('.modal__frame--ingest, #ingPage');
    if (!host) return;
    var lvl = btn.getAttribute('data-ing-lvl');
    var logs = Array.prototype.filter.call(host.querySelectorAll('[data-ing-lines]'), atBottom);
    host.setAttribute('data-inglvl', lvl);
    logs.forEach(function(el){ el.scrollTop = el.scrollHeight; });
    if (lvl !== 'dbg' || !window.htmx) return;
    host.querySelectorAll('[data-ing-lines]:not([data-dbg]):not([data-dbg-req])').forEach(function(el){
      var m = /^run-(\d+)-log$/.exec(el.id);
      if (!m) return;
      el.setAttribute('data-dbg-req', '');
      var done = function(){ el.removeAttribute('data-dbg-req'); };
      htmx.ajax('GET', '/ingestion/runs/' + m[1] + '/log?full=1', { source: el, target: '#' + el.id, swap: 'outerHTML' })
        .then(done, done);
    });
  });
})();

// Live clocks. The server renders a running run's age in [data-elapsed-ms] with
// its text; one page-wide interval advances each from the browser's monotonic
// clock, counted from when the element arrived (a swap or the page load brings
// a new element, which restarts from the server's value). It ticks four times a
// second so the text changes on true second boundaries.
(function(){
  var seen = new WeakMap();
  function fmt(n){
    var m = Math.floor(n / 60), h = Math.floor(m / 60);
    if (n < 60) return n + 's';
    if (m < 60) return m + 'm ' + String(n % 60).padStart(2, '0') + 's';
    return h + 'h ' + String(m % 60).padStart(2, '0') + 'm';
  }
  function tick(){
    var now = performance.now();
    document.querySelectorAll('[data-elapsed-ms]').forEach(function(el){
      var base = parseInt(el.getAttribute('data-elapsed-ms'), 10);
      if (isNaN(base)) return;
      var s = seen.get(el);
      if (!s || s.base !== base){
        s = { base: base, t0: now };
        seen.set(el, s);
      }
      var text = fmt(Math.floor((base + now - s.t0) / 1000));
      if (el.textContent !== text) el.textContent = text;
    });
  }
  setInterval(tick, 250);
  document.body.addEventListener('htmx:afterSwap', tick);
  document.body.addEventListener('htmx:oobAfterSwap', tick);
  tick();
})();

// Ingestion page rows: a click anywhere on a run row presses its toggle. Links
// and buttons keep their own action, and a click that ends a text selection
// only selects.
document.addEventListener('click', function(e){
  var row = e.target.closest('.in-table > tbody > .in-r');
  if (!row || e.detail > 1 || e.target.closest('a, button')) return;
  var sel = window.getSelection && window.getSelection();
  if (sel && !sel.isCollapsed && sel.toString() !== '') return;
  var tog = row.querySelector('.in-tog__b');
  if (tog) tog.click();
});

// Requests for the Ingestion page's runs carry the picked log level, so a log
// opened or refreshed under debug keeps its debug lines. A run-edge refresh
// re-renders every row, so it also names the rows that are open and the server
// renders those open again.
document.body.addEventListener('htmx:configRequest', function(e){
  if (e.detail.path.indexOf('/ingestion/') !== 0) return;
  var page = document.getElementById('ingPage');
  var polling = /\/log\?after=/.test(e.detail.path);
  if (!polling && page && page.getAttribute('data-inglvl') === 'dbg') e.detail.parameters.debug = '1';
  if (e.detail.path.indexOf('/ingestion/live') !== 0) return;
  var ids = [];
  document.querySelectorAll('.in-table > tbody[id^="run-"]').forEach(function(tb){
    var b = tb.querySelector('.in-tog__b');
    if (b && b.getAttribute('aria-expanded') === 'true') ids.push(tb.id.slice(4));
  });
  if (ids.length) e.detail.parameters.open = ids.join(',');
});

// A finished ingest leaves the feed on screen stale — it was rendered before
// the new items landed. The hidden #ingWatch span carries the live running
// flag and is OOB-replaced by every ingest response (status polls while the
// runner modal is open, its own 3s poll while it's closed), so watching that
// flag flip 1 -> 0 is the "run just finished" edge on any page. Either edge
// is also raised as a sources-ingest-change event.
(function(){
  var watch = document.getElementById('ingWatch');
  if (!watch) return;
  var wasRunning = watch.dataset.running === '1';
  var pending = false;

  // Only the feed lists ingested items; its filters live in the query string,
  // so a plain reload lands back on the same view with the new items in it.
  function refresh(){
    pending = false;
    if (document.getElementById('pane')) window.location.reload();
  }
  function check(){
    var el = document.getElementById('ingWatch');
    if (!el) return;
    var running = el.dataset.running === '1';
    // A run starting or ending anywhere (this page's bar, the topbar, the
    // modal) is told to the Sources pages, whose live parts then refresh
    // themselves (see templates/sources.html).
    if (wasRunning !== running){
      document.body.dispatchEvent(new CustomEvent('sources-ingest-change'));
    }
    // Don't yank the page out from under an open runner modal — the user is
    // reading the log. Hold the refresh until they close it.
    if (wasRunning && !running){
      if (document.getElementById('modal').firstElementChild) pending = true;
      else refresh();
    }
    wasRunning = running;
  }
  document.body.addEventListener('htmx:afterSwap', check);
  document.body.addEventListener('htmx:oobAfterSwap', check);
  document.addEventListener('modal:dismiss', function(){
    if (pending) refresh();
  });
})();

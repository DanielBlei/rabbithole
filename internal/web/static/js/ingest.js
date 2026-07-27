// Ambient ingest wiring: the runner modal's log pane, and the page-wide refresh
// that fires when a run finishes.

// Ingest runner log: keep the tail in view. Every poll swap replaces the
// whole modal body, so re-pin the log container to its bottom after swaps.
document.body.addEventListener('htmx:afterSwap', function(){
  var lines = document.querySelector('[data-ing-lines]');
  if (lines) lines.scrollTop = lines.scrollHeight;
});

// A finished ingest leaves the feed on screen stale — it was rendered before
// the new items landed. The hidden #ingWatch span carries the live running
// flag and is OOB-replaced by every ingest response (status polls while the
// runner modal is open, its own 3s poll while it's closed), so watching that
// flag flip 1 -> 0 is the "run just finished" edge on any page.
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

// Ingest log level filter (info/debug pills). The picked level lives as a
// data attribute on the modal *frame* — outside the 2s poll's swap target —
// so it survives every body refresh; the CSS does the actual hiding.
document.addEventListener('click', function(e){
  var pill = e.target.closest('[data-ing-lvl]');
  if (!pill) return;
  var frame = pill.closest('.modal__frame--ingest');
  if (frame) frame.setAttribute('data-inglvl', pill.getAttribute('data-ing-lvl'));
});

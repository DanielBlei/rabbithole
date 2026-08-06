// The Sources page: instant client-side filtering of the feed list, and the
// copy button on the export modal.
//
// Filtering is client-side because the whole set is already on the page and a
// personal feed list is tens of rows, not thousands — the same call the Feed
// page's filters make. The search box lives outside #srcBody, so an edit that
// repaints the columns leaves the query intact; this file just re-applies it
// after every swap.
(function () {
  var state = 'all'; // all | on | off | gone — kept here so it survives htmx swaps

  function query() {
    var input = document.querySelector('[data-src-query]');
    return input ? input.value.trim().toLowerCase() : '';
  }

  // A row matches on its name, its URL or any of its tags — the three things
  // you'd actually remember about a feed.
  function matches(row, q) {
    if (!q) return true;
    var hay = [row.dataset.name, row.dataset.url, row.dataset.tags].join(' ').toLowerCase();
    return hay.indexOf(q) !== -1;
  }

  function apply() {
    var q = query();
    var rows = document.querySelectorAll('[data-src-row]');
    var shown = 0, live = 0, gone = 0;
    rows.forEach(function (row) {
      var deleted = row.dataset.state === 'gone';
      if (deleted) gone++; else live++;
      // Deleted feeds sit in the list with the rest but only surface when asked
      // for — "all" means the whole live set, not the archive with it.
      var inState = deleted ? state === 'gone' : (state === 'all' || row.dataset.state === state);
      var hit = inState && matches(row, q);
      row.classList.toggle('srow--filtered', !hit);
      if (hit) shown++;
    });

    var none = document.querySelector('[data-src-none]');
    if (none) {
      none.hidden = shown !== 0 || !rows.length;
      none.textContent = (state === 'gone' && !q) ? 'nothing deleted' : 'nothing matches that search';
    }

    // Always rendered, even unnarrowed: a tally that came and went would move
    // the search box every time the filter changed. Deleted feeds count against
    // their own total — they were never part of the live set.
    var count = document.querySelector('[data-src-matches]');
    if (count) count.textContent = shown + ' of ' + (state === 'gone' ? gone : live);
  }

  // Plain http is accepted, so the warning has to be live: you want it while
  // typing the address, not after a run has already fetched it in the clear.
  function flagInsecure() {
    var input = document.querySelector('[data-src-url]');
    var warn = document.querySelector('[data-src-insecure]');
    if (input && warn) warn.hidden = !/^\s*http:\/\//i.test(input.value);
  }

  document.addEventListener('input', function (e) {
    if (e.target.matches('[data-src-query]')) apply();
    if (e.target.matches('[data-src-url]')) flagInsecure();
  });

  // The button wears the chosen state, so it reads without opening; the amber
  // dot is the bar's shared "this is narrowed" signal.
  document.addEventListener('change', function (e) {
    if (!e.target.matches('[data-src-state]')) return;
    state = e.target.value;

    var label = document.querySelector('[data-src-state-label]');
    var chip = e.target.parentNode.querySelector('span');
    if (label && chip) label.textContent = chip.textContent;
    var dot = document.querySelector('[data-src-state-dot]');
    if (dot) dot.hidden = state === 'all';

    // A pick is a decision — leave the bar clear rather than the menu hanging.
    var menu = e.target.closest('details');
    if (menu) menu.open = false;
    apply();
  });

  // Esc clears the search rather than leaving you to select-all-delete.
  document.addEventListener('keydown', function (e) {
    if (e.key !== 'Escape') return;
    var input = e.target.closest && e.target.closest('[data-src-query]');
    if (input && input.value) { e.stopPropagation(); input.value = ''; apply(); }
  });

  // Copy the exported YAML. Falls back to selecting it when the clipboard API
  // isn't available (it needs a secure context, and this serves over plain
  // HTTP on loopback), so the button always does something useful.
  document.addEventListener('click', function (e) {
    var btn = e.target.closest('[data-copy]');
    if (!btn) return;
    var target = document.querySelector(btn.dataset.copy);
    if (!target) return;
    var done = function () {
      var was = btn.textContent;
      btn.textContent = 'copied';
      setTimeout(function () { btn.textContent = was; }, 1200);
    };
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(target.textContent).then(done, function () { select(target); });
    } else {
      select(target);
    }
  });

  function select(el) {
    var range = document.createRange();
    range.selectNodeContents(el);
    var sel = window.getSelection();
    sel.removeAllRanges();
    sel.addRange(range);
  }

  function refresh() { apply(); flagInsecure(); }

  document.addEventListener('DOMContentLoaded', refresh);
  document.body.addEventListener('htmx:afterSwap', refresh);
})();

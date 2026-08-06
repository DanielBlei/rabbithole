// The Sources dialog: instant client-side filtering and paging of the feed
// list, and the copy button on the export modal.
//
// Both are client-side because the whole set is already in the dialog and a
// personal feed list is tens of rows, not thousands — the same call the Feed
// page's filters make. The search box lives outside #srcBody, so an edit that
// repaints the columns leaves the query intact; this file just re-applies it
// after every swap — including the one that opens the dialog.
//
// Loaded on every page, since the dialog opens from anywhere. Every lookup is
// guarded, so it costs nothing on a page where none of this markup exists.
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

    paginate();
  }

  // ---- paging ---------------------------------------------------------
  // Over what the filters leave in play, so paging and filtering compose: 24
  // feeds narrowed to 6 is one page, not three with gaps. The page number and
  // the chosen size live here rather than in the markup, because the pager sits
  // inside #srcBody and is replaced by every mutation.
  //
  // A set size rather than one fitted to the window: the dialog's height follows
  // from it (--src-per drives .src__cols in the stylesheet), so it opens the
  // same every time instead of tracking whatever screen you happen to be on.
  var SIZES = [10, 15, 20];
  var page = 1;
  var perPage = 10;

  try {
    var saved = parseInt(localStorage.getItem('rh.srcPerPage'), 10);
    if (SIZES.indexOf(saved) !== -1) perPage = saved;
  } catch (e) { /* private mode — the default stands */ }

  // On <html>, not on anything inside the dialog: a mutation replaces #srcBody
  // and would take the variable with it.
  function stampSize() {
    document.documentElement.style.setProperty('--src-per', perPage);
  }
  stampSize();

  function visibleRows() {
    return Array.prototype.filter.call(
      document.querySelectorAll('[data-src-row]'),
      function (row) { return !row.classList.contains('srow--filtered'); }
    );
  }

  function paginate() {
    var rows = visibleRows();
    // Un-hide first, or a row left hidden by the previous page never comes back.
    rows.forEach(function (row) { row.style.display = ''; });

    var pages = Math.max(1, Math.ceil(rows.length / perPage));
    // Clamp rather than reset — a delete on the last page should leave you as
    // close to where you were as the new set allows.
    if (page > pages) page = pages;
    if (page < 1) page = 1;

    var start = (page - 1) * perPage;
    var end = Math.min(start + perPage, rows.length);
    rows.forEach(function (row, i) {
      if (i < start || i >= end) row.style.display = 'none';
    });

    // The arrows go when one page holds the lot, or when the filters left
    // nothing at all and the zero-state is speaking instead. The size picker
    // beside them stays either way.
    var nav = document.querySelector('[data-src-pagenav]');
    if (nav) {
      nav.hidden = pages < 2;
      var label = nav.querySelector('[data-src-pagelbl]');
      if (label) label.textContent = (start + 1) + '–' + end + ' of ' + rows.length;
      nav.querySelectorAll('[data-src-page]').forEach(function (btn) {
        btn.disabled = Number(btn.dataset.srcPage) < 0 ? page === 1 : page === pages;
      });
    }

    document.querySelectorAll('[data-src-per]').forEach(function (btn) {
      btn.classList.toggle('is-on', Number(btn.dataset.srcPer) === perPage);
    });
  }

  document.addEventListener('click', function (e) {
    var step = e.target.closest('[data-src-page]');
    if (step && !step.disabled) {
      e.preventDefault();
      page += Number(step.dataset.srcPage);
      paginate();
      return;
    }
    var size = e.target.closest('[data-src-per]');
    if (!size) return;
    e.preventDefault();
    perPage = Number(size.dataset.srcPer);
    try { localStorage.setItem('rh.srcPerPage', perPage); } catch (err) { /* not fatal */ }
    stampSize();
    // A bigger page can hold what you were looking at further up the list, so
    // start again from the top rather than land somewhere arbitrary.
    page = 1;
    paginate();
  });

  // Plain http is accepted, so the warning has to be live: you want it while
  // typing the address, not after a run has already fetched it in the clear.
  function flagInsecure() {
    var input = document.querySelector('[data-src-url]');
    var warn = document.querySelector('[data-src-insecure]');
    if (input && warn) warn.hidden = !/^\s*http:\/\//i.test(input.value);
  }

  // Narrowing the set invalidates the page you were on — page 4 of the old set
  // may not exist in the new one, so a filter change lands you back at the top.
  // Only filters do this; a mutation clamps instead (see paginate).
  function refilter() {
    page = 1;
    apply();
  }

  document.addEventListener('input', function (e) {
    if (e.target.matches('[data-src-query]')) refilter();
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
    refilter();
  });

  // Esc clears the search rather than leaving you to select-all-delete. It has
  // to stop the dialog's own Esc handler too: both are bound on document, where
  // stopPropagation does nothing to a sibling listener. This file is loaded
  // ahead of modal.js so this one runs first.
  document.addEventListener('keydown', function (e) {
    if (e.key !== 'Escape') return;
    var input = e.target.closest && e.target.closest('[data-src-query]');
    if (input && input.value) { e.stopImmediatePropagation(); input.value = ''; refilter(); }
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

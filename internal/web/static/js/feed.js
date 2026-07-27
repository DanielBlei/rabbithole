// The feed page: the filter bar, client-side source/tag filtering, the interim
// pager, and the row-leaves-view animation.

// The filter bar has no submit button — every control submits on change, and
// each one carries data-filter naming what its change should do. Delegated on
// document so it keeps working on a re-rendered pane.
document.addEventListener('change', function(e){
  var el = e.target.closest && e.target.closest('[data-filter]');
  if (!el) return;
  var kind = el.getAttribute('data-filter');
  if (kind === 'window'){
    // A preset window replaces whatever custom range the form still holds.
    document.getElementById('filterFrom').value = '';
    document.getElementById('filterTo').value = '';
  } else if (kind === 'bookmark' && el.checked){
    // Checking Bookmarked enters the saved-library view; auto-tick all three
    // status units first so the whole library shows on entry (a saved item may
    // be unread, seen, or hidden). The user narrows from there by unchecking a
    // status.
    ['unread','seen','hidden'].forEach(function(n){
      var b = el.form.querySelector('input[name="' + n + '"]');
      if (b) b.checked = true;
    });
  }
  el.form.submit();
});

// Mockup-era client-side pagination, kept only so this page is interactive to
// evaluate. Replaced by real server-side after/before/limit paging + htmx once
// backend work starts — see .claude/frontend-goals.md. Global (not in an IIFE)
// so the tag filter and the row-leave animation can re-render it.
var feedPager = (function(){
  var PAGE_SIZE = 10;
  var page = 1;

  var pane = document.getElementById('pane');
  var btnFirst = document.getElementById('btnFirst');
  var tops = [document.getElementById('btnPrev'), document.getElementById('btnNext')];
  var bottoms = [document.getElementById('btnPrevBottom'), document.getElementById('btnNextBottom')];
  var infos = [document.getElementById('pageInfo'), document.getElementById('pageInfoBottom')];

  // Read the DOM on every render rather than caching a row list: rows leave
  // (a mutation moved one out of the active view) and come back replaced by
  // htmx swaps, and a swapped-in row arrives without the display the pager
  // had set on the element it replaced.
  function allRows(){ return Array.prototype.slice.call(document.querySelectorAll('.pane__body .row')); }

  function render(){
    var rows = allRows();
    // The pager works over what the tag filter leaves in play, so paging and
    // filtering compose: 30 rows filtered down to 12 is two pages, not three
    // with gaps.
    var shown = rows.filter(function(row){ return !row.classList.contains('row--filtered'); });
    var total = shown.length;
    var totalPages = Math.max(1, Math.ceil(total / PAGE_SIZE));
    if (page > totalPages) page = totalPages;
    var start = (page - 1) * PAGE_SIZE;
    var end = Math.min(start + PAGE_SIZE, total);

    rows.forEach(function(row){ row.style.display = 'none'; });
    shown.forEach(function(row, i){
      if (i >= start && i < end) row.style.display = '';
    });

    var label = total ? 'items ' + (start + 1) + '–' + end + ' of ' + total : 'no items';
    // The server caps the render at listLimit, so say when there are matches
    // below the fold that no amount of paging will reach.
    var limit = parseInt(pane.dataset.limit, 10);
    var avail = parseInt(pane.dataset.available, 10);
    if (rows.length >= limit && avail > rows.length) label += ' · first ' + limit + ' of ' + avail;
    infos.forEach(function(el){ if (el) el.textContent = label; });
    if (btnFirst) btnFirst.disabled = page === 1;
    tops.concat(bottoms).forEach(function(btn, i){
      if (btn) btn.disabled = (i % 2 === 0) ? page === 1 : page === totalPages;
    });
  }

  function goTo(target){
    page = Math.max(1, target);
    render();
    var reduced = window.matchMedia('(prefers-reduced-motion: reduce)').matches;
    pane.scrollIntoView({ behavior: reduced ? 'auto' : 'smooth', block: 'start' });
  }

  if (btnFirst) btnFirst.addEventListener('click', function(){ goTo(1); });
  tops.concat(bottoms).forEach(function(btn, i){
    if (btn) btn.addEventListener('click', function(){ goTo(page + (i % 2 === 0 ? -1 : 1)); });
  });

  render();
  return { render: render, goTo: goTo };
})();

(function(){
  // Source and tag filters: client-side, over the rows already on the page
  // (each row carries its feed in data-source and that feed's tags in
  // data-tags). Both chip sets are server-rendered from what those rows
  // actually hold, so there's nothing to reconcile here — selecting none in a
  // set means that set doesn't filter.
  var tags = new Set();
  var srcs = new Set();

  // A row survives both filters independently: it must be from a selected
  // source (or none picked) AND carry a selected tag (or none picked). Within
  // each, selections are OR — "Medium — AI or Red Hat, tagged AI".
  function apply(){
    document.querySelectorAll('.pane__body .row').forEach(function(row){
      var okSrc = !srcs.size || srcs.has(row.dataset.source || '');
      var rowTags = (row.dataset.tags || '').split(',').filter(Boolean);
      var okTag = !tags.size || rowTags.some(function(t){ return tags.has(t); });
      row.classList.toggle('row--filtered', !(okSrc && okTag));
    });
    mark('[data-tag-active]', tags);
    mark('[data-src-active]', srcs);
    wearSelection('[data-src-label]', '[data-src-more]', srcs, 'All');
    wearSelection('[data-tag-label]', '[data-tag-more]', tags, 'All');
  }

  function mark(sel, set){
    var dot = document.querySelector(sel);
    if (dot) dot.hidden = set.size === 0;
  }

  // The closed Source and Tags buttons carry their selection, so both have to
  // survive names far wider than the button: the first name is shown and
  // ellipsised by CSS if it's extravagant, with the "+N" for the rest kept in
  // its own element so that truncation can never eat the count. The full list
  // is the hover title. empty is what the button reads when nothing is picked,
  // matching each menu's own unnarrowed chip — "All" for both.
  function wearSelection(labelSel, moreSel, set, empty){
    var el = document.querySelector(labelSel);
    var more = document.querySelector(moreSel);
    if (!el) return;
    var picked = Array.from(set);
    el.textContent = picked.length ? picked[0] : empty;
    // Left in the layout even when empty — CSS reserves its slot, so the label
    // doesn't shift as the count comes and goes.
    if (more) more.textContent = picked.length > 1 ? '+' + (picked.length - 1) : '';
    var btn = el.closest('.popmenu__btn');
    if (btn) btn.title = picked.length ? picked.join(', ') : '';
  }

  // Wires one filter's chips: the "all" chip clears the set (it can't be turned
  // off directly — unchecking it would mean what it already means), and it
  // tracks the selection rather than being a chip of its own.
  function bind(allSel, chkSel, set){
    var all = document.querySelector(allSel);
    var boxes = document.querySelectorAll(chkSel);
    function sync(){
      if (all) all.checked = set.size === 0;
      apply();
      // The visible set changed under the pager; page 1 is the only sane
      // landing spot (page 4 of the old set may not exist in the new one).
      feedPager.goTo(1);
    }
    if (all) all.addEventListener('change', function(){
      set.clear();
      boxes.forEach(function(box){ box.checked = false; });
      sync();
    });
    boxes.forEach(function(box){
      box.addEventListener('change', function(){
        if (box.checked) set.add(box.value); else set.delete(box.value);
        sync();
      });
    });
  }

  bind('.tag-filter__all', '.tag-filter__chk', tags);
  bind('.src-filter__all', '.src-filter__chk', srcs);
  apply();

  // A row htmx swaps back in is a fresh element: it has neither the filter
  // class nor the display the pager gave its predecessor, so reapply both.
  document.addEventListener('htmx:afterSwap', function(){ apply(); feedPager.render(); });
})();

(function(){
  // Delegated on document so these keep working on rows htmx swaps in after a
  // note save (per-element listeners bound at load wouldn't reach a fresh row).

  // why/note switcher: clicking the already-open tab collapses the shared box.
  // Panels show via CSS :checked; radios can't uncheck themselves, so this only
  // handles the collapse-on-re-click case. State is stashed on the tab element
  // between its own mousedown and click (same element, so it survives).
  document.addEventListener('mousedown', function(e){
    var tab = e.target.closest('.tg-tab');
    if (!tab) return;
    var input = document.getElementById(tab.getAttribute('for'));
    tab._open = !!(input && input.checked);
  });
  document.addEventListener('click', function(e){
    var tab = e.target.closest('.tg-tab');
    if (!tab || !tab._open) return;
    var input = document.getElementById(tab.getAttribute('for'));
    if (input){ e.preventDefault(); input.checked = false; }
  });

  // Apply persists via the form's hx-post and htmx swaps in the server-rendered
  // row (already in view mode with the saved note) — no optimistic update here.
  // Cancel just restores the textarea to the last saved text (the view <p>) in
  // case the user typed and changed their mind without applying.
  document.addEventListener('click', function(e){
    var cancel = e.target.closest('.note-cancel');
    if (!cancel) return;
    var panel = cancel.closest('.tg-panel-note');
    if (!panel) return;
    var input = panel.querySelector('.note-input');
    var viewP = panel.querySelector('.note-view p');
    if (input) input.value = panel.classList.contains('empty') ? '' : viewP.textContent;
  });
})();

(function(){
  // After a seen/hide mutation re-renders a row, if its new status falls outside
  // the active View filter (e.g. you hid an item while not viewing hidden), hold
  // it a beat so the change registers, then animate it out — rather than leaving
  // it lingering dimmed in a list it no longer belongs to. Unread always stays.
  function viewState(){
    function on(n){ var el = document.querySelector('input[name="' + n + '"]'); return !!(el && el.checked); }
    return { unread: on('unread'), seen: on('seen'), hidden: on('hidden'), bookmarked: on('bookmarked') };
  }
  function outOfView(row, vs){
    // A row stays only if its status unit is currently shown AND, when the
    // bookmark filter is on, it's still saved — so marking a status the active
    // chips exclude, or un-bookmarking within the library view, drops it.
    var statusShown = row.classList.contains('is-hidden') ? vs.hidden
      : row.classList.contains('is-seen') ? vs.seen
      : vs.unread;
    if (!statusShown) return true;
    if (vs.bookmarked){
      var bm = row.querySelector('.bookmark');
      if (!(bm && bm.classList.contains('is-saved'))) return true;
    }
    return false;
  }
  // Avg Score is the mean over the rows currently shown (data-score is present
  // only on scored rows, matching the server's score() gate). Recomputed from
  // the live DOM when a row leaves, so the tile tracks the visible set without
  // a reload. Tweened so the number eases to its new value as the row goes.
  function currentAvg(){
    var sum = 0, n = 0;
    document.querySelectorAll('.pane__body .row').forEach(function(row){
      var s = row.getAttribute('data-score');
      if (s !== null){ sum += parseFloat(s); n++; }
    });
    return n ? sum / n : 0;
  }
  function tweenAvg(){
    var el = document.getElementById('statAvg');
    if (!el) return;
    var to = currentAvg(), from = parseFloat(el.textContent);
    if (isNaN(from)) from = to;
    var reduced = window.matchMedia('(prefers-reduced-motion: reduce)').matches;
    if (reduced || from === to){ el.textContent = to.toFixed(1); return; }
    var start = performance.now(), dur = 350;
    requestAnimationFrame(function step(now){
      var t = Math.min(1, (now - start) / dur);
      el.textContent = (from + (to - from) * t).toFixed(1);
      if (t < 1) requestAnimationFrame(step);
    });
  }
  // High Signal tile: count of visible scored rows in the high tier, recomputed
  // from the live DOM whenever a row leaves so it tracks the shown set without a
  // reload. The tier bound comes off the tile, so Go stays its only definition.
  function setHigh(){
    var el = document.getElementById('statHigh');
    if (!el) return;
    var min = parseFloat(el.dataset.high);
    var n = 0;
    document.querySelectorAll('.pane__body .row').forEach(function(row){
      var s = row.getAttribute('data-score');
      if (s !== null && parseFloat(s) >= min) n++;
    });
    el.textContent = n;
  }
  function remove(row){ row.remove(); tweenAvg(); setHigh(); feedPager.render(); }
  function leave(row){
    if (row._leaving) return;
    row._leaving = true;
    var reduced = window.matchMedia('(prefers-reduced-motion: reduce)').matches;
    setTimeout(function(){
      if (reduced){ remove(row); return; }
      row.style.maxHeight = row.scrollHeight + 'px';
      requestAnimationFrame(function(){ row.classList.add('row--leaving'); });
      setTimeout(function(){ remove(row); }, 400);
    }, 450);
  }
  // Scan after every swap: only the just-mutated row can now fall out of view
  // (the rest were server-rendered to match the filter), so this stays cheap.
  document.addEventListener('htmx:afterSwap', function(){
    var vs = viewState();
    document.querySelectorAll('.pane__body .row').forEach(function(row){
      if (outOfView(row, vs)) leave(row);
    });
  });
})();

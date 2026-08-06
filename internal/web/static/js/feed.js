// The feed page: the filter bar, the search preview, the interim pager, and the
// row-leaves-view animation.

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
  } else if (kind === 'all'){
    // "All" is the absence of a selection rather than a value of its own, so it
    // clears its menu's chips and submits without them. Unchecking it directly
    // would mean what it already means, so it just puts itself back.
    if (!el.checked){ el.checked = true; return; }
    el.closest('.popmenu__panel').querySelectorAll('input[type="checkbox"]').forEach(function(box){
      if (box !== el) box.checked = false;
    });
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

  // Back to the first page without the scroll goTo does: for filtering that
  // happens while you type, being yanked to the top of the pane on every
  // keystroke is worse than not moving at all.
  function reset(){ page = 1; render(); }

  if (btnFirst) btnFirst.addEventListener('click', function(){ goTo(1); });
  tops.concat(bottoms).forEach(function(btn, i){
    if (btn) btn.addEventListener('click', function(){ goTo(page + (i % 2 === 0 ? -1 : 1)); });
  });

  render();
  return { render: render, goTo: goTo, reset: reset };
})();

(function(){
  // The search control. Every filter in the bar is a store query now, this one
  // included — what's left here is the preview it shows while you type, over
  // the rows already on the page, so narrowing feels instant and Enter goes and
  // gets the rest.

  // The search text is read from the field rather than kept in a variable: the
  // server renders the submitted search back into it, so the field is where the
  // query lives on arrival as well as while typing.
  function searchText(){
    var el = document.querySelector('[data-search-query]');
    return el ? el.value.trim().toLowerCase() : '';
  }

  // What a row is searched on: its title, its feed and that feed's tags — the
  // three things you'd remember about an item, and the same three the server
  // matches on so typing previews what Enter will return.
  function haystack(row){
    var title = row.querySelector('.title');
    return ((title ? title.textContent : '') + ' ' +
      (row.dataset.source || '') + ' ' + (row.dataset.tags || '')).toLowerCase();
  }

  // The rows the server sent are already narrowed by every other control, so
  // this only has to hide the ones the text doesn't match.
  function apply(){
    var search = searchText();
    var rows = document.querySelectorAll('.pane__body .row');
    var shown = 0;
    rows.forEach(function(row){
      var hit = !search || haystack(row).indexOf(search) !== -1;
      row.classList.toggle('row--filtered', !hit);
      if (hit) shown++;
    });
    wearSearch();
    sayNothing(shown, rows.length, search);
  }

  // The closed search button wears its query, like Source and Tags wear theirs —
  // the menu is shut most of the time, and a filter you can't see is one you
  // forget is on.
  function wearSearch(){
    var el = document.querySelector('[data-search-label]');
    if (!el) return;
    var input = document.querySelector('[data-search-query]');
    var typed = input ? input.value.trim() : '';
    // Empty label, not a placeholder word: the button is a bare magnifier until
    // there's something to say, and CSS hides the span while it holds nothing.
    el.textContent = typed;
    var btn = el.closest('.popmenu__btn');
    if (btn) btn.title = typed ? 'Searching for "' + typed + '"' : 'Search title, source or tag';
    var dot = document.querySelector('[data-search-active]');
    if (dot) dot.hidden = !typed;
    // Nothing to clear on an empty field.
    var clear = document.querySelector('[data-search-clear]');
    if (clear) clear.hidden = !typed;
  }

  // Filtering every row out of view leaves an empty body: the server's
  // zero-state only covers a render that came back with nothing. When the
  // render was capped, say so rather than claiming nothing matches — the item
  // being looked for may be further down the window, which is what Enter is for.
  function sayNothing(shown, total, search){
    var el = document.querySelector('[data-search-none]');
    if (!el) return;
    el.hidden = shown !== 0 || total === 0;
    if (el.hidden) return;
    var pane = document.getElementById('pane');
    var limit = parseInt(pane.dataset.limit, 10);
    var avail = parseInt(pane.dataset.available, 10);
    if (search && total >= limit && avail > total){
      el.textContent = 'no match in the ' + total + ' items loaded · ↵ searches all ' + avail;
    } else {
      el.textContent = search ? 'nothing matches that search' : 'nothing matches those filters';
    }
  }

  var searchMenu = document.querySelector('.popmenu--search');
  var searchInput = document.querySelector('[data-search-query]');
  var searchSugg = document.querySelector('[data-search-sugg]');
  var activeOpt = -1; // index into the suggestions, -1 = none

  // Completion covers what the page can enumerate: the feeds and tags its rows
  // carry, read off the chips the server already rendered for the other two
  // filters. Titles are matched while typing but never suggested — there are as
  // many as there are items, and a list of them is the feed itself.
  function vocabulary(){
    var out = [];
    document.querySelectorAll('.src-filter__chk').forEach(function(box){
      out.push({ kind: 'source', value: box.value });
    });
    document.querySelectorAll('.tag-filter__chk').forEach(function(box){
      out.push({ kind: 'tag', value: box.value });
    });
    return out;
  }

  function suggest(){
    if (!searchSugg) return;
    var text = searchText();
    var hits = [];
    if (text){
      vocabulary().forEach(function(item){
        var lower = item.value.toLowerCase();
        var at = lower.indexOf(text);
        // Nothing to complete once it's typed in full.
        if (at !== -1 && lower !== text) hits.push({ item: item, at: at });
      });
      // A name that starts with what was typed is the likelier target than one
      // that merely contains it somewhere.
      hits.sort(function(a, b){ return a.at - b.at; });
      hits = hits.slice(0, 6);
    }
    activeOpt = -1;
    searchSugg.textContent = '';
    hits.forEach(function(hit, i){
      // Built as nodes rather than markup: feed names are user text. Options,
      // not buttons — the field keeps focus and the arrows move
      // aria-activedescendant, which is what a combobox's list is.
      var opt = document.createElement('div');
      opt.className = 'search__opt';
      opt.id = 'searchOpt' + i;
      opt.setAttribute('role', 'option');
      opt.setAttribute('aria-selected', 'false');
      opt.dataset.searchOpt = hit.item.value;
      var kind = document.createElement('span');
      kind.className = 'search__opt-kind';
      kind.textContent = hit.item.kind;
      var value = document.createElement('span');
      value.className = 'search__opt-val';
      value.textContent = hit.item.value;
      opt.appendChild(kind);
      opt.appendChild(value);
      searchSugg.appendChild(opt);
    });
    searchSugg.hidden = !hits.length;
    if (searchInput) searchInput.setAttribute('aria-expanded', hits.length ? 'true' : 'false');
    highlight();
  }

  function options(){
    return searchSugg ? Array.prototype.slice.call(searchSugg.querySelectorAll('.search__opt')) : [];
  }

  function highlight(){
    var opts = options();
    opts.forEach(function(opt, i){
      opt.classList.toggle('is-active', i === activeOpt);
      opt.setAttribute('aria-selected', i === activeOpt ? 'true' : 'false');
    });
    if (searchInput) {
      searchInput.setAttribute('aria-activedescendant', opts[activeOpt] ? opts[activeOpt].id : '');
    }
  }

  function closeSuggestions(){
    if (searchSugg) searchSugg.hidden = true;
    activeOpt = -1;
    if (searchInput) searchInput.setAttribute('aria-expanded', 'false');
    highlight();
  }

  // Back to the unnarrowed feed, from the ✕ or from Escape. Clearing a search
  // the server ran has to go back for the full set; clearing one that was only
  // ever typed is a local change, so it costs no round trip.
  function clearSearch(){
    if (!searchInput) return;
    searchInput.value = '';
    if (searchInput.dataset.searchSubmitted){ searchInput.form.submit(); return; }
    apply();
    closeSuggestions();
    feedPager.reset();
    // Only when the panel is open: the bar's ✕ clears without opening it, and
    // pulling focus into a collapsed panel would be a jump to nowhere.
    if (searchMenu && searchMenu.open) searchInput.focus();
  }

  if (searchInput){
    // Typing only ever filters what's loaded. Emptying the field deliberately
    // doesn't go back to the server either: Enter and Escape are the two ways
    // to run a search, so mid-edit keystrokes can never trigger a navigation.
    searchInput.addEventListener('input', function(){
      apply();
      suggest();
      // The visible set changed under the pager, but without goTo's scroll:
      // being pulled to the top of the pane on every keystroke is worse than
      // staying put.
      feedPager.reset();
    });

    // Bound on the input, not on document, so it runs before chrome.js's
    // Escape (which closes every popmenu) and can stop the event reaching it —
    // Escape unwinds this control a step at a time instead of shutting it.
    searchInput.addEventListener('keydown', function(e){
      var opts = options();
      if ((e.key === 'ArrowDown' || e.key === 'ArrowUp') && opts.length){
        e.preventDefault();
        activeOpt += e.key === 'ArrowDown' ? 1 : -1;
        if (activeOpt >= opts.length) activeOpt = -1;   // past the end, back to the raw text
        if (activeOpt < -1) activeOpt = opts.length - 1;
        highlight();
        return;
      }
      if (e.key === 'Enter'){
        // The control sits inside the GET filter form, so Enter would submit
        // anyway — but with the highlighted suggestion left out of the field.
        e.preventDefault();
        if (opts[activeOpt]) searchInput.value = opts[activeOpt].dataset.searchOpt;
        searchInput.form.submit();
        return;
      }
      if (e.key === 'Escape'){
        if (searchSugg && !searchSugg.hidden){
          e.preventDefault();
          e.stopPropagation();
          closeSuggestions();
          return;
        }
        if (searchInput.value){
          e.preventDefault();
          e.stopPropagation();
          clearSearch();
        }
      }
    });
  }

  var searchClear = document.querySelector('[data-search-clear]');
  if (searchClear) searchClear.addEventListener('click', clearSearch);

  // A suggestion is a decision: fill the field and search on it.
  if (searchSugg) searchSugg.addEventListener('click', function(e){
    var opt = e.target.closest('.search__opt');
    if (!opt || !searchInput) return;
    searchInput.value = opt.dataset.searchOpt;
    searchInput.form.submit();
  });

  // Opening the menu should land in the field — the magnifier is the only
  // reason to open it.
  if (searchMenu) searchMenu.addEventListener('toggle', function(){
    if (searchMenu.open && searchInput){ searchInput.focus(); searchInput.select(); }
    else closeSuggestions();
  });

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

(function(){
  // First-run hint pointing at the side menu. The server only renders #navHint
  // on the feed while no ingest has ever run, so this just decides whether the
  // user has already seen it — one flag, no expiry, dismissed for good on the
  // first interaction of any kind.
  var hint = document.getElementById('navHint');
  if (!hint) return;
  var KEY = 'rh.tour.ingest';
  var tab = document.getElementById('navTab');

  function dismiss(){
    try { localStorage.setItem(KEY, '1'); } catch (e) { /* private mode: show it again, no worse */ }
    hint.remove();
    if (tab) tab.classList.remove('navtab--hint');
  }

  var seen;
  try { seen = localStorage.getItem(KEY); } catch (e) { seen = null; }
  if (seen){ hint.remove(); return; }

  hint.hidden = false;
  if (tab) tab.classList.add('navtab--hint');
  // Next frame, so the entrance transition has a starting state to move from.
  requestAnimationFrame(function(){ hint.classList.add('navhint--on'); });

  // Anything the user does retires it: the ✕, a click anywhere, Escape, or
  // reaching for the menu it points at (hovering the edge zone opens the
  // drawer, which is the hint's whole job).
  document.addEventListener('click', dismiss);
  document.addEventListener('keydown', function(e){ if (e.key === 'Escape') dismiss(); });
  var hot = document.getElementById('navHot');
  if (hot) hot.addEventListener('mouseenter', dismiss);
})();

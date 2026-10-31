// The Sources manage page: instant client-side search, filtering and sorting of
// the source list, the inline editors (dirty state, discard guard, live hints,
// reveal), the attention block's collapse, the copy button on the export
// modal, and the import preview (picker, drop or sample) with its select-all
// and apply label.
//
// Client-side because the whole set is already on the page and a personal
// source list is tens of rows, not thousands, the same call the Feed page's
// filters make. The search box, filter pills and the tag and sort menus live
// outside #srcBody, so an edit that repaints the list leaves the query intact;
// this file re-applies it after every swap, including the one that first shows
// the list. The pill counts, tag menu, summary line and attention block are
// re-sent by the server after a mutation; their state (which pill or tag is
// picked, whether the block is open) is kept here so it survives those swaps.
// An editor lives in its source's unit (#src-{id}), which the server swaps
// alone; closing one is done here by removing it, so no request is made.
//
// The add form starts with a find step: Find feeds posts the address typed in
// the find input (no name, so it is never part of the form), the candidate list
// comes back as a fragment, and picking a row fills the details step and its
// preview. See "the add form's find step" below.
//
// Ticked rows are kept in a Set, not in the rows, so they survive a swap; it
// only ever holds rows that are on screen. The bulk bar acts on it: the server
// renders the bar and its buttons, this file shows it, builds the Remove tag
// menu from the ticked rows' own tags and adds their ids to each request.
//
// Loaded on every page by the layout. Every lookup is guarded, so it costs
// nothing on a page where none of this markup exists.
(function () {
  var FILTERS = ['all', 'enabled', 'disabled', 'failing', 'flaky', 'removed'];
  var EMPTY = {
    enabled: 'No enabled sources. Turn one on in the list.',
    disabled: 'Nothing is disabled.',
    failing: 'Nothing is failing.',
    flaky: 'Nothing is flaky.',
    removed: 'Nothing removed. Removed sources wait here so you can restore them.'
  };
  var SORTS = ['name', 'items', 'signal', 'last'];
  var filter = 'all';
  var tag = ''; // '' is all tags; else the menu option's own spelling
  var sort = 'name';
  var attnOpen = null; // decided the first time the block is on screen
  var scrollOnOpen = false; // the next editor to arrive is one the user asked for
  var focusField = null; // 'url' when Edit URL asked for the address
  var flashAfter = null; // unit to flash once its swap lands
  var refocusForm = null; // the editor whose form was just submitted
  var sel = new Set(); // ids of the ticked rows, always a subset of the visible ones
  var selQueued = false;
  var tagOpen = false; // the bulk bar's Add tag input is showing
  var rmKey = null; // the tags the Remove tag menu was built from
  var focusSel = false; // a bulk Remove or Restore is in flight: refocus when the selection empties
  var ovFoldPrefix = 'rh.sources.overview.fold.';

  function input() { return document.querySelector('[data-src-query]'); }
  function query() {
    var el = input();
    return el ? el.value.trim().toLowerCase() : '';
  }

  // A row has a tag when it is one of its comma-joined tags, in any case.
  function hasTag(row, t) {
    if (!t) return true;
    return (row.dataset.tags || '').toLowerCase().split(',').indexOf(t.toLowerCase()) !== -1;
  }

  // A row matches on its name, address, tags or kind: the things you'd
  // actually remember about a source.
  function matches(row, q) {
    if (!q) return true;
    var hay = [row.dataset.name, row.dataset.url, row.dataset.tags, row.dataset.type].join(' ').toLowerCase();
    return hay.indexOf(q) !== -1;
  }

  // Removed sources only show under their own pill; every other pill is about
  // the live set. Health states come from the row's data-state.
  function inFilter(row, f) {
    var removed = row.dataset.removed === '1';
    if (f === 'removed') return removed;
    if (removed) return false;
    var st = row.dataset.state;
    switch (f) {
      case 'enabled': return st !== 'off';
      case 'disabled': return st === 'off';
      case 'failing': return st === 'error';
      case 'flaky': return st === 'flaky';
      default: return true;
    }
  }

  function showEmpty(name, text) {
    document.querySelectorAll('[data-src-empty]').forEach(function (el) {
      el.hidden = el.dataset.srcEmpty !== name;
      if (!el.hidden && text !== undefined) {
        var t = el.querySelector('[data-src-empty-text]');
        if (t) t.textContent = text;
      }
    });
  }

  function byName(a, b) {
    return a.dataset.name.localeCompare(b.dataset.name, undefined, {sensitivity: 'base'});
  }

  // Descending by a numeric attribute, rows without a value after the rest.
  function byNumber(attr) {
    return function (a, b) {
      var x = a.dataset[attr], y = b.dataset[attr];
      if ((x === '') !== (y === '')) return x === '' ? 1 : -1;
      return (Number(y) || 0) - (Number(x) || 0) || byName(a, b);
    };
  }

  // Puts the rows in the picked order. The header row stays first and nothing
  // moves while an editor is open, focused or not, so a save that drops focus
  // does not carry the open unit and its notice out of view. closeEditor calls
  // apply once the editor is gone, which sorts again. Rows already in order are
  // left alone, so focus and animations survive.
  function sortRows() {
    var list = document.querySelector('[data-src-list]');
    if (!list) return;
    if (list.querySelector(':scope > .mg-item .mg-ed')) return;
    var rows = Array.prototype.slice.call(list.querySelectorAll(':scope > .mg-item'));
    var cmp = sort === 'items' ? byNumber('perweek')
      : sort === 'signal' ? byNumber('signal')
      : sort === 'last' ? byNumber('last') : byName;
    var sorted = rows.slice().sort(cmp);
    if (sorted.every(function (row, i) { return row === rows[i]; })) return;
    sorted.forEach(function (row) { list.appendChild(row); });
  }

  // Tags: each row shows the chips its column has room for and counts the rest
  // in a "+N" chip whose title names them. Chips do not shrink, so the fit is
  // measured on their natural widths. A lone chip still too wide is cut with an
  // ellipsis. A hidden row has no width, so it is fitted when a filter shows it.
  function fitTagRow(row) {
    var chips = [].slice.call(row.querySelectorAll('.tag[data-tag]'));
    var more = row.querySelector('[data-tag-more]');
    if (!more || !chips.length || !row.clientWidth) return;
    var gap = parseFloat(getComputedStyle(row).columnGap) || 0;
    var over = function () { return row.scrollWidth > row.clientWidth + 1; };
    var show = function (k) {
      chips.forEach(function (c, i) { c.hidden = i >= k; c.style.maxWidth = ''; });
      more.hidden = k === chips.length;
      more.textContent = '+' + (chips.length - k);
      more.title = chips.slice(k).map(function (c) { return c.dataset.tag; }).join(', ');
    };
    var k = chips.length;
    show(k);
    while (k > 1 && over()) { k--; show(k); }
    if (over()) {
      var room = row.clientWidth - (more.hidden ? 0 : more.offsetWidth + gap);
      chips[0].style.maxWidth = Math.max(0, room) + 'px';
    }
  }

  function fitTags() {
    document.querySelectorAll('[data-tag-fit]').forEach(fitTagRow);
    document.querySelectorAll('.mg-listwrap').forEach(function (el) { tagResize.observe(el); });
  }
  var tagResize = window.ResizeObserver ? new ResizeObserver(fitTags) : { observe: function () {} };

  function apply() {
    sortRows();
    var q = query();
    var rows = document.querySelectorAll('[data-src-row]');
    var shown = 0, live = 0, removedN = 0;
    rows.forEach(function (row) {
      if (row.dataset.removed === '1') removedN++; else live++;
      // A source being edited with unsaved changes stays in view, whatever the
      // search says; the pills ask before hiding it (see the change handler).
      var form = row.querySelector(':scope > .mg-ed');
      var hit = (form && isDirty(form)) || (inFilter(row, filter) && hasTag(row, tag) && matches(row, q));
      row.hidden = !hit;
      if (hit) shown++;
    });
    syncSel();
    fitTags();

    var toolbar = document.querySelector('[data-src-toolbar]');
    if (toolbar) toolbar.hidden = rows.length === 0;
    var x = document.querySelector('.mg-search__x[data-src-clear]');
    if (x) x.hidden = !q;

    var count = document.querySelector('[data-src-matches]');
    if (count) {
      count.hidden = !q;
      count.textContent = shown + ' of ' + (filter === 'removed' ? removedN : live);
    }

    // With no rows at all the server has drawn the first-run state itself.
    if (!rows.length) return;
    if (shown) showEmpty(null);
    else if (q || tag) showEmpty('search');
    else if (filter === 'all' && !live) showEmpty('first');
    else showEmpty('filter', EMPTY[filter]);
  }

  function syncPills() {
    document.querySelectorAll('[data-src-state]').forEach(function (r) { r.checked = r.value === filter; });
  }

  // The menu option whose value is t, in any case, or '' when there is none.
  function tagOption(t) {
    var menu = document.getElementById('srcTagMenu');
    if (!t || !menu) return '';
    var opts = menu.querySelectorAll('[role="option"]');
    for (var i = 0; i < opts.length; i++) {
      var v = opts[i].dataset.value;
      if (v && v.toLowerCase() === t.toLowerCase()) return v;
    }
    return '';
  }

  // Checks the radio of a menu, which the listbox shows. The change it fires
  // finds nothing new to do once the state is already set.
  function pick(name, value) {
    var radios = document.querySelectorAll('input[type="radio"][name="' + name + '"]');
    for (var i = 0; i < radios.length; i++) {
      if (radios[i].value === value && !radios[i].checked) {
        radios[i].checked = true;
        radios[i].dispatchEvent(new Event('change', {bubbles: true}));
      }
    }
  }

  // The menus follow the state after a swap: the server sends the tag menu
  // with nothing picked, and a tag no live source has any more is dropped.
  function syncMenus() {
    var was = tag;
    tag = tagOption(tag);
    pick('srctag', tag);
    pick('srcsort', sort);
    if (tag !== was) syncURL();
  }

  // The pill, tag and sort are the state; the address carries them so a link or
  // a reload lands on the same view.
  function syncURL() {
    try {
      var u = new URL(window.location.href);
      if (filter === 'all') u.searchParams.delete('state'); else u.searchParams.set('state', filter);
      if (!tag) u.searchParams.delete('tag'); else u.searchParams.set('tag', tag);
      if (sort === 'name') u.searchParams.delete('sort'); else u.searchParams.set('sort', sort);
      u.searchParams.delete('select');
      history.replaceState(null, '', u.pathname + u.search + u.hash);
    } catch (e) { /* not fatal */ }
  }

  function setFilter(f) {
    filter = FILTERS.indexOf(f) === -1 ? 'all' : f;
    syncPills();
    apply();
    syncURL();
  }

  function setTag(t) {
    t = tagOption(t);
    if (t === tag) return;
    tag = t;
    apply();
    syncURL();
  }

  function setSort(s) {
    s = SORTS.indexOf(s) === -1 ? 'name' : s;
    if (s === sort) return;
    sort = s;
    apply();
    syncURL();
  }

  function clearAll() {
    var el = input();
    if (el) { el.value = ''; el.focus(); }
    tag = '';
    pick('srctag', '');
    apply();
    syncURL();
  }

  // ---- selection and the bulk bar ----------------------------------------
  function bar() { return document.querySelector('[data-src-bulk]'); }
  function rowID(row) { return row.id.slice(4); }
  function visibleRows() {
    return Array.prototype.filter.call(document.querySelectorAll('#srcBody [data-src-row]'), function (row) { return !row.hidden; });
  }

  // The union of the ticked rows' own tags, one spelling each, sorted.
  function ownTags() {
    var seen = {}, out = [];
    sel.forEach(function (id) {
      var unit = document.getElementById('src-' + id);
      if (!unit) return;
      (unit.dataset.ownTags || '').split(',').forEach(function (t) {
        t = t.trim();
        if (t && !seen[t.toLowerCase()]) { seen[t.toLowerCase()] = true; out.push(t); }
      });
    });
    return out.sort(function (a, b) { return a.localeCompare(b, undefined, {sensitivity: 'base'}); });
  }

  function node(tagName, cls) {
    var el = document.createElement(tagName);
    if (cls) el.className = cls;
    return el;
  }

  // The Remove tag menu: the markup of partials/listbox.html, built here
  // because its options depend on the ticked rows. The first option is the
  // button's label and stays hidden; picking a tag resets the menu to it.
  function tagMenu(tags) {
    var m = node('div', 'stp-menu stp-menu--inline');
    m.id = 'srcBulkRm';
    m.setAttribute('data-stp-menu', '');
    m.setAttribute('data-default', '');
    var b = node('button', 'stp-menu__btn');
    b.type = 'button';
    b.setAttribute('data-stp-menubtn', '');
    b.setAttribute('aria-haspopup', 'listbox');
    b.setAttribute('aria-expanded', 'false');
    b.setAttribute('aria-label', 'Remove tag from the selected sources');
    var cur = node('span', 'stp-menu__cur');
    cur.setAttribute('data-stp-menucur', '');
    cur.textContent = 'Remove tag';
    b.appendChild(cur);
    b.insertAdjacentHTML('beforeend', '<svg viewBox="0 0 12 12" width="12" height="12" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M3 4.5l3 3 3-3"/></svg>');
    var ul = node('ul', 'stp-menu__list');
    ul.setAttribute('role', 'listbox');
    ul.tabIndex = -1;
    ul.setAttribute('aria-label', 'Remove tag');
    ul.hidden = true;
    var radios = node('div', 'stp-sr');
    radios.hidden = true;
    [''].concat(tags).forEach(function (v) {
      var li = node('li');
      li.setAttribute('role', 'option');
      li.dataset.value = v;
      li.dataset.label = 'Remove tag';
      li.setAttribute('aria-selected', 'false');
      li.hidden = !v;
      var n = node('span', 'stp-menu__n');
      n.textContent = v || 'Remove tag';
      li.appendChild(n);
      ul.appendChild(li);
      var r = node('input');
      r.type = 'radio';
      r.name = 'srcbulkrm';
      r.value = v;
      r.checked = !v;
      r.setAttribute('aria-label', v || 'Remove tag');
      radios.appendChild(r);
    });
    m.appendChild(b);
    m.appendChild(ul);
    m.appendChild(radios);
    return m;
  }

  function paintRm(tags) {
    var host = document.querySelector('[data-src-bulk-rm]');
    if (!host) return;
    var key = tags.join('\n');
    if (key === rmKey) return;
    rmKey = key;
    host.replaceChildren();
    host.hidden = !tags.length;
    if (!tags.length) return;
    host.appendChild(tagMenu(tags));
    if (window.rhListbox) window.rhListbox.init(host);
  }

  function paintBar(n) {
    var el = bar();
    if (!el) return;
    el.hidden = n === 0;
    var removed = filter === 'removed';
    if (!n || removed) tagOpen = false;
    var count = el.querySelector('[data-src-bulk-n]');
    if (count) count.textContent = n ? n + ' selected' : '';
    if (!n) {
      el.parentNode.style.setProperty('--mg-bulk-h', '0px');
      return;
    }
    var live = el.querySelector('[data-src-bulk-live]');
    var gone = el.querySelector('[data-src-bulk-removed]');
    if (live) live.hidden = removed;
    if (gone) gone.hidden = !removed;
    var box = el.querySelector('[data-src-bulk-tagbox]');
    var open = el.querySelector('[data-src-bulk-tag]');
    if (box) box.hidden = !tagOpen;
    if (open) open.hidden = tagOpen;
    paintRm(removed ? [] : ownTags());
    el.parentNode.style.setProperty('--mg-bulk-h', el.offsetHeight + 8 + 'px');
  }

  // Drops ids whose row is gone or hidden, then repaints every tick, the
  // select-all box and the bar from the Set.
  function paintSel() {
    var rows = visibleRows();
    var seen = {};
    rows.forEach(function (row) { seen[rowID(row)] = true; });
    sel.forEach(function (id) { if (!seen[id]) sel.delete(id); });
    var n = 0;
    document.querySelectorAll('#srcBody [data-src-row]').forEach(function (row) {
      var on = sel.has(rowID(row));
      if (on) n++;
      var box = row.querySelector('[data-src-pick]');
      if (box) box.checked = on;
      var line = row.querySelector(':scope > .mg-row');
      if (line) line.classList.toggle('is-sel', on);
    });
    var all = document.querySelector('[data-src-all]');
    if (all) {
      all.disabled = rows.length === 0;
      all.checked = rows.length > 0 && n === rows.length;
      all.indeterminate = n > 0 && n < rows.length;
    }
    paintBar(n);
    if (!n && focusSel) { focusSel = false; focusSelAll(); }
  }

  // oobAfterSwap fires once per swapped row, so repaints wait for the frame.
  function syncSel() {
    if (selQueued) return;
    selQueued = true;
    requestAnimationFrame(function () { selQueued = false; paintSel(); });
  }

  function focusSelAll() {
    var all = document.querySelector('[data-src-all]');
    var target = all && !all.disabled ? all : input();
    if (target) target.focus();
  }

  function clearSel() {
    sel.clear();
    paintSel();
  }

  function openTagBox(open) {
    tagOpen = open;
    paintBar(sel.size);
    var el = bar();
    var target = el && el.querySelector(open ? '[data-src-bulk-tagin]' : '[data-src-bulk-tag]');
    if (target) target.focus({preventScroll: true});
  }

  // A bulk action would act on a row whose editor has unsaved changes.
  function bulkGuard(run) {
    var dirty = dirtyEditor();
    var unit = dirty && unitOf(dirty);
    if (dirty && unit && sel.has(unit.id.slice(4))) showGuard(dirty, run);
    else run();
  }

  // net.js asks before an Undo that would put back a source whose editor has
  // unsaved changes: ids are the sources the undo covers.
  window.rhSourcesGuard = function (ids, run) {
    var dirty = dirtyEditor();
    var unit = dirty && unitOf(dirty);
    if (dirty && unit && ids.indexOf(unit.id.slice(4)) !== -1) showGuard(dirty, run);
    else run();
  };

  // The deep link from the Overview: ?select=id,id ticks those sources, with
  // every filter back on All so none is hidden.
  function selectFromURL(raw) {
    var ids = raw.split(',').filter(function (id) {
      return /^[\w-]+$/.test(id) && document.getElementById('src-' + id);
    });
    if (!ids.length) return;
    var field = input();
    if (field) field.value = '';
    tag = '';
    pick('srctag', '');
    filter = 'all';
    syncPills();
    apply();
    sel.clear();
    ids.forEach(function (id) { sel.add(id); });
    paintSel();
    syncURL();
    var first = document.getElementById('src-' + ids[0]);
    if (first && !first.hidden) {
      scrollToUnit(first, 'start');
      flashRow(ids[0]);
    }
  }

  // ---- needs-attention block --------------------------------------------
  // Open by default, closed on a phone where its names line would crowd the
  // list. The server always sends it open; this applies the state after each
  // swap so a collapse survives a mutation.
  function syncAttn() {
    var toggle = document.querySelector('[data-src-attn]');
    if (!toggle) return;
    if (attnOpen === null) attnOpen = !(window.matchMedia && window.matchMedia('(max-width: 520px)').matches);
    toggle.setAttribute('aria-expanded', attnOpen ? 'true' : 'false');
    var body = document.getElementById('mg-attn-body');
    if (body) body.hidden = !attnOpen;
  }

  // ---- editors ----------------------------------------------------------
  // The editor is a form in its unit. Dirty means its fields differ from what
  // they held when it arrived; Save and Reset wait for that.
  function serialize(form) { return new URLSearchParams(new FormData(form)).toString(); }
  function isDirty(form) { return form.dataset.discard !== '1' && serialize(form) !== form.dataset.base; }
  function unitOf(form) { return form.closest('.mg-item'); }
  function isAddForm(form) { return !!form.closest('.mg-item--add'); }

  function dirtyEditor() {
    var forms = document.querySelectorAll('#srcBody .mg-ed');
    for (var i = 0; i < forms.length; i++) if (isDirty(forms[i])) return forms[i];
    return null;
  }

  // What a tuning field will do, worded as the server words it
  // (tuningHint in internal/web/sources.go).
  function tuning(typed, own, fallback, from) {
    if (!typed) return fallback + ' ' + from;
    if (typed === own) return typed + ' set on this source';
    return typed + ' set here (otherwise ' + fallback + ' ' + from + ')';
  }

  function paint(form) {
    var dirty = isDirty(form);
    var save = form.querySelector('[data-src-save]');
    var reset = form.querySelector('[data-src-reset]');
    if (save) save.disabled = !dirty;
    if (reset) reset.disabled = !dirty;
    var label = form.querySelector('[data-src-dirty]');
    if (label) label.textContent = dirty ? 'Unsaved changes' : '';
    var notice = form.querySelector('[data-src-notice]');
    if (notice && dirty) notice.hidden = true;
    var pill = form.querySelector('[name="enabled"]:checked');
    form.querySelectorAll('[data-src-fetch-hint]').forEach(function (hint) {
      hint.hidden = (hint.dataset.srcFetchHint === 'default') !== (!!pill && pill.value === '');
    });
    form.querySelectorAll('[data-src-hint]').forEach(function (hint) {
      var field = form.querySelector(hint.dataset.srcHint === 'since' ? '[name="since"]' : '[name="max_items"]');
      var t = hint.querySelector('[data-src-hint-t]');
      if (field && t) t.textContent = tuning(field.value.trim(), hint.dataset.own, hint.dataset.fallback, hint.dataset.from);
    });
    var url = form.querySelector('[data-src-url]');
    var warn = form.querySelector('[data-src-insecure]');
    if (url && warn) warn.hidden = !/^\s*http:\/\//i.test(url.value);
    // Find and Preview read feeds; a saved search on an academic provider is not one.
    var type = form.querySelector('[name="type"]:checked');
    var academic = !!type && type.value === 'academic';
    form.querySelectorAll('[data-src-rss-only]').forEach(function (el) {
      el.disabled = academic || el.classList.contains('htmx-request');
    });
  }

  function focusEditor(form, which) {
    var field = form.querySelector(which === 'find' ? '[data-src-find]' : which === 'url' ? '[name="url"]' : '[name="name"]');
    if (!field) return;
    field.focus({preventScroll: true});
    try { field.select(); } catch (e) { /* not selectable */ }
  }

  function scrollToUnit(unit, block) {
    if (unit && unit.scrollIntoView) unit.scrollIntoView({block: block || 'start'});
  }

  // Closing is local: the editor goes, and the row's button goes back to Edit.
  // The button keeps its hx-get, so a later Edit fetches a fresh editor.
  function closeEditor(form) {
    var unit = unitOf(form);
    if (!unit) return;
    if (isAddForm(form)) { clearTimeout(findTimer); unit.remove(); return; }
    dropFeedParam(unit.id.slice(4));
    form.remove();
    var row = unit.querySelector('.mg-row');
    if (row) row.classList.remove('is-open');
    var btn = unit.querySelector('.mg-edit');
    if (btn) {
      var name = unit.dataset.name;
      btn.setAttribute('aria-expanded', 'false');
      btn.setAttribute('aria-label', 'Edit ' + name);
      btn.textContent = 'Edit';
      btn.removeAttribute('data-src-close');
      btn.setAttribute('data-src-open', '');
    }
    // sortRows skipped while the editor was open; re-apply the sort and filter now.
    apply();
  }

  // ?feed= names the source the page opened on; once its editor is closed the
  // address no longer describes the page.
  function dropFeedParam(id) {
    try {
      var u = new URL(window.location.href);
      if (u.searchParams.get('feed') !== id) return;
      u.searchParams.delete('feed');
      history.replaceState(null, '', u.pathname + u.search + u.hash);
    } catch (e) { /* not fatal */ }
  }

  function removeGuard() {
    document.querySelectorAll('.mg-guard').forEach(function (g) { g.remove(); });
  }

  // "Discard changes?" under the row of the dirty editor. run is what the user
  // was trying to do; it goes ahead only on Discard.
  function showGuard(form, run) {
    removeGuard();
    var guard = document.createElement('div');
    guard.className = 'mg-guard';
    guard.setAttribute('role', 'group');
    guard.setAttribute('aria-label', 'Unsaved changes');
    var q = document.createElement('span');
    q.className = 'mg-guard__q';
    q.appendChild(document.createTextNode('Discard changes to ' + (form.querySelector('[name="name"]').value.trim() || 'this source') + '? '));
    var hint = document.createElement('span');
    hint.className = 'sh-hint';
    hint.textContent = 'They have not been saved.';
    q.appendChild(hint);
    var acts = document.createElement('span');
    acts.className = 'mg-guard__a';
    var discard = document.createElement('button');
    discard.type = 'button';
    discard.className = 'clk-btn clk-btn--danger';
    discard.textContent = 'Discard';
    var keep = document.createElement('button');
    keep.type = 'button';
    keep.className = 'clk-btn';
    keep.textContent = 'Keep editing';
    acts.appendChild(discard);
    acts.appendChild(keep);
    guard.appendChild(q);
    guard.appendChild(acts);
    form.parentNode.insertBefore(guard, form);

    discard.addEventListener('click', function () {
      form.dataset.discard = '1';
      removeGuard();
      run();
    });
    keep.addEventListener('click', function () {
      removeGuard();
      flashAfter = null;
      focusField = null;
      focusEditor(form, 'name');
      scrollToUnit(unitOf(form), 'center');
    });
    scrollToUnit(unitOf(form), 'center');
    keep.focus({preventScroll: true});
  }

  // Runs what the user asked for, unless it would throw away a dirty editor.
  // except is the editor the action belongs to, which is not at risk.
  function guardThen(run, except) {
    var dirty = dirtyEditor();
    if (dirty && dirty !== except) showGuard(dirty, run);
    else run();
  }

  // A new editor arrived (open, save, reset, add, or the page itself): set up
  // its state, close any other, and put focus where the user was working.
  function initEditors() {
    document.querySelectorAll('#srcBody .mg-ed:not([data-ready])').forEach(function (form) {
      form.dataset.ready = '1';
      // A form re-rendered with an error holds typed values that were never
      // saved, so it starts out dirty.
      form.dataset.base = form.querySelector('.sdet__err') ? '' : serialize(form);
      paint(form);
      document.querySelectorAll('#srcBody .mg-ed').forEach(function (other) {
        if (other !== form && other.dataset.ready) closeEditor(other);
      });
      var unit = unitOf(form);
      if (scrollOnOpen) scrollToUnit(unit, 'start');
      if (scrollOnOpen || refocusForm === form.dataset.srcEd || isAddForm(form)) {
        // An add form still at its find step starts in the find input.
        var step = form.querySelector('[data-src-step2]');
        var which = scrollOnOpen ? focusField : null;
        if (isAddForm(form) && step && step.hidden) which = 'find';
        focusEditor(form, which);
      }
      scrollOnOpen = false;
      focusField = null;
      refocusForm = null;
    });
    if (flashAfter) { flashRow(flashAfter); flashAfter = null; }
  }

  function flashRow(id) {
    var unit = document.getElementById('src-' + id);
    var row = unit && unit.querySelector('.mg-row');
    if (!row) return;
    row.classList.remove('is-flash');
    void row.offsetWidth;
    row.classList.add('is-flash');
    setTimeout(function () { row.classList.remove('is-flash'); }, 1900);
  }

  // Bring a source into view and open its editor. The filter follows the
  // source: the current one if it holds it, else All (Removed for a removed
  // source). A search that hides it is cleared.
  function reveal(id, which) {
    var unit = document.getElementById('src-' + id);
    if (!unit) return;
    var removed = unit.dataset.removed === '1';
    var want = removed ? 'removed' : (inFilter(unit, filter) ? filter : 'all');
    var field = input();
    if (field && !matches(unit, query())) field.value = '';
    if (!hasTag(unit, tag)) { tag = ''; pick('srctag', ''); syncURL(); }
    if (want !== filter) setFilter(want); else apply();
    // A removed source has no editor: it is only brought to the middle.
    if (removed) {
      flashRow(id);
      scrollToUnit(unit, 'center');
      return;
    }
    var form = unit.querySelector(':scope > .mg-ed');
    if (form) {
      flashRow(id);
      scrollToUnit(unit, 'start');
      focusEditor(form, which);
      return;
    }
    var btn = unit.querySelector('[data-src-open]');
    if (!btn) return;
    flashAfter = id;
    focusField = which || null;
    btn.click();
  }

  // Runs before htmx sees a click, so a guarded action can be held back and
  // replayed on Discard.
  document.addEventListener('click', function (e) {
    var t = e.target;

    var openEditor = document.querySelector('#srcBody .mg-item:not(.mg-item--add) > .mg-ed');
    if (openEditor && !openEditor.contains(t) && !t.closest('.mg-row') && !t.closest('.mg-guard')) {
      e.preventDefault();
      e.stopImmediatePropagation();
      guardThen(function () { closeEditor(openEditor); }, null);
      return;
    }
    if (!t.closest) return;

    // Add tag with nothing typed makes no request.
    if (t.closest('[data-src-bulk-tagbox] [data-src-bulk-act]')) {
      var typed = document.querySelector('[data-src-bulk-tagin]');
      if (typed && !typed.value.replace(/,/g, '').trim()) {
        e.preventDefault();
        e.stopImmediatePropagation();
        if (window.rhToast) window.rhToast.show('Tag name is required.');
        typed.focus({preventScroll: true});
        return;
      }
    }

    // A menu closes on any scroll, so a Remove tag button clipped by the bar's
    // own scroll is brought fully into view first and the click replayed.
    var rmBtn = t.closest('[data-src-bulk-rm] [data-stp-menubtn]');
    if (rmBtn && rmBtn.dataset.srcFit !== '1') {
      var strip = bar();
      var sr = strip.getBoundingClientRect();
      var br = rmBtn.getBoundingClientRect();
      if (br.left < sr.left || br.right > sr.right) {
        e.preventDefault();
        e.stopImmediatePropagation();
        strip.scrollLeft += br.left < sr.left ? br.left - sr.left - 8 : br.right - sr.right + 8;
        rmBtn.dataset.srcFit = '1';
        requestAnimationFrame(function () {
          requestAnimationFrame(function () {
            rmBtn.click();
            delete rmBtn.dataset.srcFit;
          });
        });
        return;
      }
    }

    if (t.closest('[data-src-bulk-tag]')) { openTagBox(true); return; }
    if (t.closest('[data-src-bulk-tagx]')) { openTagBox(false); return; }
    if (t.closest('[data-src-bulk-clear]')) { clearSel(); focusSelAll(); return; }

    var toggle = t.closest('[data-src-attn]');
    if (toggle) {
      attnOpen = toggle.getAttribute('aria-expanded') !== 'true';
      syncAttn();
      return;
    }

    var rev = t.closest('[data-src-reveal]');
    if (rev) {
      e.preventDefault();
      reveal(rev.dataset.srcReveal, rev.dataset.srcFocus);
      return;
    }

    var close = t.closest('[data-src-close]');
    if (close) {
      e.preventDefault();
      e.stopImmediatePropagation();
      var form = close.closest('.mg-ed') || (close.closest('.mg-item') || document).querySelector(':scope > .mg-ed');
      if (form) guardThen(function () { closeEditor(form); }, null);
      return;
    }

    if (t.closest('[data-src-clear]')) {
      var el = input();
      if (el) { el.value = ''; el.focus(); }
      apply();
      return;
    }
    if (t.closest('[data-src-clearall]')) { clearAll(); return; }
    if (t.closest('[data-src-showall]')) { setFilter('all'); return; }

    // Anything that would replace or close a dirty editor asks first: opening
    // another source, adding, a switch on the source being edited, or its own
    // Remove. Other controls inside the editor, and in dialogs, are their own
    // business.
    var act = t.closest('[data-src-open],[data-src-add],[data-src-repaint],[data-src-import],[data-src-bulk-act],[hx-target]');
    if (!act || (act.closest('.mg-ed') && !act.matches('[data-src-remove]')) || act.closest('#modal,#modalTop')) return;
    var opens = act.matches('[data-src-open]');
    var target = act.getAttribute('hx-target') || '';
    var unit = /^#src-[\w-]+$/.test(target) ? document.querySelector(target) : null;
    var own = unit && unit.querySelector(':scope > .mg-ed');
    var dirty = dirtyEditor();
    var dirtyUnit = dirty && unitOf(dirty);
    var replaces = opens || target === '#srcBody' || (own && own === dirty) ||
      act.matches('[data-src-add],[data-src-repaint],[data-src-import]') ||
      (act.matches('[data-src-bulk-act]') && !!dirtyUnit && sel.has(dirtyUnit.id.slice(4)));
    if (dirty && replaces && act.dataset.srcReplay !== '1') {
      e.preventDefault();
      e.stopImmediatePropagation();
      showGuard(dirty, function () {
        act.dataset.srcReplay = '1';
        act.click();
        delete act.dataset.srcReplay;
      });
      return;
    }
    if (opens) scrollOnOpen = true;
  }, true);

  document.body.addEventListener('htmx:configRequest', function (e) {
    var target = e.detail.target;
    var path = e.detail.path || '';
    // Find posts the find input, Preview the Address field.
    if (path === '/sources/discover' || path === '/sources/preview') {
      var owner = e.detail.elt && e.detail.elt.closest && e.detail.elt.closest('.mg-ed');
      var src = owner && owner.querySelector(path === '/sources/discover' ? '[data-src-find]' : '[data-src-url]');
      e.detail.parameters.url = src ? src.value.trim() : '';
    }
    // A switch pressed while its editor is open keeps the editor open.
    if (target && /\/enabled$/.test(path) && target.querySelector(':scope > .mg-ed')) e.detail.parameters.open = '1';
    // The bulk bar acts on the ticked rows.
    var from = e.detail.elt;
    if (/^\/sources\/(bulk|export)$/.test(path) && from && from.closest && from.closest('[data-src-bulk]')) {
      e.detail.parameters.ids = Array.from(sel).join(',');
      if (e.detail.parameters.action === 'addtag') {
        var tagin = document.querySelector('[data-src-bulk-tagin]');
        e.detail.parameters.tag = tagin ? tagin.value.trim() : '';
      }
      var act = e.detail.parameters.action;
      if (act === 'remove' || act === 'restore') focusSel = true;
    }
    // A live refresh leaves out the rows being edited.
    if (/^\/sources\/live\b/.test(path)) {
      var ids = [];
      document.querySelectorAll('#srcBody .mg-item:not(.mg-item--add) > .mg-ed').forEach(function (f) {
        ids.push(f.closest('.mg-item').id.slice(4));
      });
      e.detail.parameters.open = ids.join(',');
    }
  });

  // A bulk request is over: the Add tag input closes once the tag went in, and
  // a Remove or Restore that changed nothing leaves focus where it was.
  document.body.addEventListener('htmx:afterRequest', function (e) {
    var from = e.detail.elt;
    if (!from || !from.closest || !from.closest('[data-src-bulk]')) return;
    if (e.detail.successful && from.closest('[data-src-bulk-tagbox]')) {
      var tagin = document.querySelector('[data-src-bulk-tagin]');
      if (tagin) tagin.value = '';
      openTagBox(false);
    }
    paintSel();
    focusSel = false;
    // The Remove tag menu is rebuilt from the rows' new tags: focus goes to the
    // new button, or to select-all when no tag is left to remove.
    if (e.detail.successful && from.matches('[data-src-bulk-rm]')) {
      var menuBtn = from.querySelector('[data-stp-menubtn]');
      if (menuBtn && !from.hidden) menuBtn.focus({preventScroll: true});
      else focusSelAll();
    }
  });

  document.body.addEventListener('htmx:beforeRequest', function (e) {
    var elt = e.detail.elt;
    var form = elt && elt.closest && elt.closest('.mg-ed');
    if (!form) return;
    var path = requestPath(e);
    if (path === '/sources/discover') startFinding(form);
    else if (path === '/sources/preview') loadingPreview(form);
    // The find controls leave the editor where it is.
    if (!FIND_PATH.test(path)) refocusForm = form.dataset.srcEd;
  });

  // Keep what the dirty state and hints show true as the editor is used. The
  // tag input changes a hidden field without an event, so clicks and keys
  // count too.
  ['input', 'change', 'click', 'keyup'].forEach(function (type) {
    document.addEventListener(type, function (e) {
      var form = e.target.closest && e.target.closest('.mg-ed');
      if (form) setTimeout(function () { if (form.isConnected) paint(form); }, 0);
    });
  });

  document.addEventListener('input', function (e) {
    if (e.target.matches('[data-src-query]')) apply();
  });

  document.addEventListener('change', function (e) {
    var t = e.target;
    if (t.matches('input[name="srctag"]')) { setTag(t.value); return; }
    if (t.matches('input[name="srcsort"]')) { setSort(t.value); return; }
    if (t.matches('[data-src-pick]')) {
      if (t.checked) sel.add(t.value); else sel.delete(t.value);
      paintSel();
    } else if (t.matches('[data-src-all]')) {
      visibleRows().forEach(function (row) {
        if (t.checked) sel.add(rowID(row)); else sel.delete(rowID(row));
      });
      paintSel();
    } else if (t.matches('input[name="srcbulkrm"]')) {
      var v = t.value;
      t.closest('[data-stp-menu]').querySelectorAll('input[name="srcbulkrm"]').forEach(function (r) { r.checked = r.value === ''; });
      if (!v) return;
      bulkGuard(function () {
        htmx.ajax('POST', '/sources/bulk', {source: document.querySelector('[data-src-bulk-rm]'), swap: 'none', values: {action: 'removetag', tag: v}});
      });
    }
  });

  document.addEventListener('keydown', function (e) {
    if (e.key !== 'Enter' || !e.target.matches || !e.target.matches('[data-src-bulk-tagin]')) return;
    e.preventDefault();
    var go = bar() && bar().querySelector('[data-src-bulk-tagbox] [data-src-bulk-act]');
    if (go) go.click();
  });

  // A pill that would hide the source being edited asks first. Keep editing
  // puts the pill back.
  document.addEventListener('change', function (e) {
    if (!e.target.matches('[data-src-state]')) return;
    var next = e.target.value;
    var form = dirtyEditor();
    var unit = form && unitOf(form);
    if (form && unit && !isAddForm(form) && !inFilter(unit, next)) {
      syncPills();
      showGuard(form, function () { setFilter(next); });
      return;
    }
    setFilter(next);
  });

  // Esc works outward: an open modal first (modal.js, which this file runs
  // ahead of, so step aside), then the bulk bar's tag input, then the editor,
  // then the search text, and last the ticked rows. From the search box the
  // text goes first: that is where the key was pressed. Both
  // modal.js and this are bound on document, where stopPropagation does nothing
  // to a sibling listener, so this file is loaded ahead of modal.js to run first.
  document.addEventListener('keydown', function (e) {
    if (e.key !== 'Escape' || e.defaultPrevented) return;
    if (document.documentElement.classList.contains('modal-open')) return;
    if (e.target.closest && e.target.closest('[data-src-bulk-tagin]')) {
      e.stopImmediatePropagation();
      openTagBox(false);
      return;
    }
    var field = e.target.closest && e.target.closest('[data-src-query]');
    if (field && field.value) {
      e.stopImmediatePropagation();
      field.value = '';
      apply();
      return;
    }
    if (e.target.closest && e.target.closest('.tagput__suggest')) return;
    var form = (e.target.closest && e.target.closest('.mg-ed')) || document.querySelector('#srcBody .mg-ed');
    if (!form) {
      if (sel.size) { e.stopImmediatePropagation(); clearSel(); }
      return;
    }
    e.stopImmediatePropagation();
    guardThen(function () { closeEditor(form); }, null);
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

  // ---- the add form's find step --------------------------------------------
  // Find feeds looks an address up for its feeds. The list that comes back holds
  // radios with no form owner (see partials/source_find.html), so picking one
  // changes nothing the form posts; pickCandidate copies the row's name, address
  // and preview into the details step. Preview reads the Address field and Find
  // the find input, both added to the request in htmx:configRequest.
  var FIND_PATH = /^\/sources\/(discover|preview|check)$/;
  var findTimer = null;

  function requestPath(e) {
    var cfg = e.detail.requestConfig;
    var path = (cfg && cfg.path) || (e.detail.pathInfo && e.detail.pathInfo.requestPath) || '';
    return path.split('?')[0];
  }

  function part(form, name) { return form.querySelector('[data-src-' + name + ']'); }
  function previewBox(form) { return form.querySelector('.mg-prev'); }

  function hostOf(raw) {
    try {
      return new URL(/^[a-z][a-z0-9+.-]*:\/\//i.test(raw) ? raw : 'https://' + raw).host || raw;
    } catch (err) { return raw; }
  }

  function spinner(small) {
    var el = node('span', 'mg-spin' + (small ? ' mg-spin--sm' : ''));
    el.setAttribute('aria-hidden', 'true');
    return el;
  }

  function setStatus(form, text) {
    var box = part(form, 'status');
    if (!box) return;
    box.replaceChildren();
    if (!text) return;
    var label = node('span');
    label.textContent = text;
    box.appendChild(spinner(false));
    box.appendChild(label);
  }

  function startFinding(form) {
    clearTimeout(findTimer);
    var cands = part(form, 'cands');
    if (cands) cands.replaceChildren();
    var find = part(form, 'find');
    var raw = find ? find.value.trim() : '';
    if (!raw) { setStatus(form, ''); return; }
    setStatus(form, 'Looking for feeds at ' + hostOf(raw) + '...');
    findTimer = setTimeout(function () {
      if (form.isConnected) setStatus(form, 'Still looking. Some sites take a few seconds to answer.');
    }, 1200);
  }

  function stopFinding(form) {
    clearTimeout(findTimer);
    setStatus(form, '');
  }

  function loadingPreview(form) {
    var box = previewBox(form);
    if (!box) return;
    var title = node('p', 'mg-prev__t');
    title.textContent = 'Recent posts';
    var line = node('p', 'sh-hint');
    line.appendChild(spinner(true));
    line.appendChild(document.createTextNode(' Loading a preview...'));
    box.replaceChildren(title, line);
  }

  // Editing the Address leaves a preview of the old one behind, so it goes, and
  // a read still in flight is dropped.
  function clearPreview(form) {
    var go = part(form, 'preview');
    if (go && go.classList.contains('htmx-request')) htmx.trigger(go, 'htmx:abort');
    var box = previewBox(form);
    if (box) box.replaceChildren();
  }

  function failedRequest(e) {
    var elt = e.detail.elt;
    var form = elt && elt.closest && elt.closest('.mg-ed');
    var path = requestPath(e);
    if (!form || (path !== '/sources/discover' && path !== '/sources/preview')) return;
    var msg = node('div', 'sdet__err' + (path === '/sources/discover' ? ' mg-find__err' : ''));
    msg.setAttribute('role', 'alert');
    msg.textContent = form.dataset.findFailed || '';
    if (path === '/sources/discover') {
      stopFinding(form);
      msg.tabIndex = -1;
      var cands = part(form, 'cands');
      if (cands) cands.replaceChildren(msg);
      msg.focus({preventScroll: true});
    } else {
      var box = previewBox(form);
      if (box) box.replaceChildren(msg);
    }
  }

  ['htmx:responseError', 'htmx:sendError', 'htmx:timeout'].forEach(function (type) {
    document.body.addEventListener(type, failedRequest);
  });

  document.body.addEventListener('htmx:afterRequest', function (e) {
    var elt = e.detail.elt;
    var form = elt && elt.closest && elt.closest('.mg-ed');
    if (!form || !elt.matches) return;
    if (e.detail.successful && elt.matches('[data-src-find-go]')) stopFinding(form);
    if (e.detail.successful && elt.matches('[data-src-restore]')) restored(elt);
    // htmx re-enables a button when its request ends, whatever the Type says.
    if (elt.matches('[data-src-rss-only]')) paint(form);
  });

  // The answer is in: a lone feed that can be added is picked for you (focus
  // lands on Name); with several, focus goes to the first, and an error takes it.
  function foundFeeds(form) {
    if (!form) return;
    var open = form.querySelectorAll('[data-src-cands] input[name="cand"]:not(:disabled)');
    if (open.length === 1) { pickCandidate(open[0]); return; }
    if (open.length > 1) { open[0].focus({preventScroll: true}); return; }
    var err = form.querySelector('[data-src-cands] .mg-find__err');
    if (err) err.focus({preventScroll: true});
  }

  function showStep2(form) {
    var step = part(form, 'step2');
    if (step) step.hidden = false;
  }

  function pickCandidate(radio) {
    var form = radio.closest('.mg-ed');
    if (!form) return;
    radio.checked = true;
    var name = form.querySelector('[name="name"]');
    var addr = form.querySelector('[name="url"]');
    var kind = form.querySelector('[name="type"][value="' + (radio.dataset.type === 'academic' ? 'academic' : 'rss') + '"]');
    if (name) name.value = radio.dataset.name || '';
    if (addr) addr.value = radio.dataset.url || '';
    if (kind) kind.checked = true;
    showStep2(form);
    var row = radio.closest('.mg-cand');
    var held = row && row.querySelector('[data-src-cand-prev]');
    clearPreview(form);
    var box = previewBox(form);
    if (box && held) Array.prototype.forEach.call(held.childNodes, function (n) { box.appendChild(n.cloneNode(true)); });
    var dup = form.querySelector('.mg-dup');
    if (dup) dup.replaceChildren();
    paint(form);
    focusEditor(form, null);
    var step = part(form, 'step2');
    if (step && step.scrollIntoView) step.scrollIntoView({block: 'nearest'});
  }

  // The details step without a find: the typed address carries over.
  function enterManually(form) {
    var find = part(form, 'find');
    var addr = form.querySelector('[name="url"]');
    showStep2(form);
    if (addr && find && !addr.value.trim() && find.value.trim()) {
      addr.value = find.value.trim();
      addr.dispatchEvent(new Event('input', {bubbles: true}));
    }
    paint(form);
    focusEditor(form, null);
  }

  // A restored feed is a source again: the note says so and offers to show it.
  function restored(btn) {
    var note = btn.closest('[data-src-note]');
    if (!note) return;
    var row = note.closest('.mg-cand');
    if (row) row.dataset.state = 'subscribed';
    var chip = node('span', 'stp-chip stchip--new');
    chip.textContent = 'Restored';
    var show = node('button', 'clk-btn clk-btn--link');
    show.type = 'button';
    show.setAttribute('data-src-reveal', btn.dataset.srcRestore);
    show.textContent = 'Show it';
    note.replaceChildren(chip, document.createTextNode(' '), show);
  }

  document.addEventListener('click', function (e) {
    var manual = e.target.closest && e.target.closest('[data-src-manual]');
    if (!manual) return;
    var form = manual.closest('.mg-ed');
    if (form) enterManually(form);
  });

  document.addEventListener('change', function (e) {
    var t = e.target;
    if (t.matches && t.matches('input[name="cand"]') && t.checked) pickCandidate(t);
  });

  document.addEventListener('input', function (e) {
    var t = e.target;
    if (!t.matches || !t.matches('[data-src-url]')) return;
    var form = t.closest('.mg-ed');
    if (form && form.dataset.srcEd === 'new') clearPreview(form);
  });

  // Enter in the find input finds. Left alone it would submit the whole form.
  document.addEventListener('keydown', function (e) {
    if (e.key !== 'Enter' || e.isComposing || !e.target.matches || !e.target.matches('[data-src-find]')) return;
    e.preventDefault();
    var go = e.target.closest('.mg-ed').querySelector('[data-src-find-go]');
    if (go && !go.disabled) go.click();
  });

  // ---- import ----------------------------------------------------------
  // The Import button opens the dialog (hx-get into #modalTop). A file, picked
  // there, dropped on the page or fetched as the sample, is posted for a
  // preview that replaces the dialog's content. The plan's apply form is an
  // ordinary htmx form; this file keeps its select-all box and button label in
  // step with the ticks.
  var SAMPLE = '/static/sources-sample.yaml';

  // The server answers with the dialog, swapped in the way an htmx request
  // would have.
  function preview(form) {
    var top = document.getElementById('modalTop');
    if (!top) return;
    fetch('/sources/import/preview', { method: 'POST', body: form, credentials: 'same-origin' })
      .then(function (res) {
        // An expired session redirects to the login page; let the gate take over
        // rather than swap that page into the dialog.
        if (res.redirected || new URL(res.url).pathname !== '/sources/import/preview') {
          window.location.reload();
          return null;
        }
        if (!res.ok) throw new Error('status ' + res.status);
        return res.text();
      })
      .then(function (html) {
        if (html === null) return;
        htmx.swap(top, html, { swapStyle: 'innerHTML' });
        if (window.tagput) window.tagput.colorChips();
        syncPlan();
      })
      .catch(function () { importError('could not read the feeds file'); });
  }

  function previewFile(file) {
    if (!file) return;
    var form = new FormData();
    form.append('file', file);
    preview(form);
  }

  function previewSample() {
    fetch(SAMPLE, { credentials: 'same-origin' })
      .then(function (res) {
        if (!res.ok) throw new Error('status ' + res.status);
        return res.text();
      })
      .then(function (text) {
        var form = new FormData();
        form.append('yaml', text);
        form.append('name', 'sources-sample.yaml');
        preview(form);
      })
      .catch(function () { importError('could not load the sample file'); });
  }

  // Said inside the open dialog, else above the list.
  function importError(msg) {
    var zone = document.getElementById('srcBody');
    if (zone) zone.classList.remove('is-drop');
    var host = document.querySelector('#modalTop .mg-imp');
    var col = document.querySelector('#srcBody .src__col');
    var parent = host || col;
    if (!parent) return;
    var old = parent.querySelector(':scope > .sdet__err');
    if (old) old.remove();
    var el = document.createElement('div');
    el.className = 'sdet__err' + (host ? '' : ' src__import');
    el.setAttribute('role', 'alert');
    el.textContent = msg;
    parent.insertBefore(el, parent.firstChild);
  }

  // The select-all box is on when every new row is ticked, mixed when some are,
  // and off with nothing to tick. The apply button says what it will do.
  function syncPlan() {
    document.querySelectorAll('[data-plan]').forEach(function (plan) {
      var all = plan.querySelector('[data-plan-all]');
      var apply = plan.querySelector('[data-plan-apply]');
      if (!all || !apply) return;
      var add = 0, addOn = 0, restore = 0;
      plan.querySelectorAll('[data-plan-pick]').forEach(function (box) {
        var status = box.closest('.plan__row').dataset.status;
        if (status === 'new') { add++; if (box.checked) addOn++; }
        else if (box.checked) restore++;
      });
      all.disabled = add === 0;
      all.checked = add > 0 && addOn === add;
      all.indeterminate = addOn > 0 && addOn < add;
      apply.textContent = addOn && restore ? 'Add ' + addOn + ', restore ' + restore
        : (restore ? 'Restore ' + restore : 'Add ' + addOn);
      apply.disabled = !addOn && !restore;
    });
  }

  document.addEventListener('click', function (e) {
    if (e.target.closest('[data-imp-pick]')) {
      var input = document.querySelector('#modalTop [data-src-file]');
      if (input) input.click();
    } else if (e.target.closest('[data-imp-sample]')) {
      previewSample();
    }
  });

  document.addEventListener('change', function (e) {
    var t = e.target;
    if (t.matches('[data-src-file]')) {
      previewFile(t.files[0]);
      t.value = '';
    } else if (t.matches('[data-plan-all]')) {
      t.closest('[data-plan]').querySelectorAll('.plan__row[data-status="new"] [data-plan-pick]').forEach(function (box) {
        box.checked = t.checked;
      });
      syncPlan();
    } else if (t.matches('[data-plan-pick]')) {
      syncPlan();
    }
  });

  // A feeds file dropped anywhere on the Sources page, over the dialog too,
  // opens its preview. The list lights up as the drop target.
  function dropZone(e) {
    return hasFiles(e) ? document.getElementById('srcBody') : null;
  }
  function hasFiles(e) {
    return e.dataTransfer && Array.prototype.indexOf.call(e.dataTransfer.types || [], 'Files') !== -1;
  }

  document.addEventListener('dragover', function (e) {
    var zone = dropZone(e);
    if (!zone) return;
    e.preventDefault();
    zone.classList.add('is-drop');
  });

  document.addEventListener('dragleave', function (e) {
    var zone = dropZone(e);
    if (zone && !e.relatedTarget) zone.classList.remove('is-drop');
  });

  document.addEventListener('drop', function (e) {
    var zone = dropZone(e);
    if (!zone) return;
    e.preventDefault();
    zone.classList.remove('is-drop');
    // The preview takes over the screen, so it asks first when an editor has
    // unsaved changes.
    var file = e.dataTransfer.files[0];
    guardThen(function () { previewFile(file); }, null);
  });

  // On narrow screens the section nav is a scrolling row. Bring the current
  // item into it; a column nav has no overflow to correct.
  function showActiveNav() {
    var el = document.querySelector('.sh-ni.is-on');
    var list = el && el.closest('.sh-nav__list');
    if (!list || getComputedStyle(list).flexDirection !== 'row') return;
    el.scrollIntoView({block: 'nearest', inline: 'nearest'});
  }

  function refreshOverviewFolds() {
    document.querySelectorAll('[data-ov-fold]').forEach(function (button) {
      var key = ovFoldPrefix + button.dataset.ovFold;
      var saved = null;
      try { saved = localStorage.getItem(key); } catch (e) {}
      var open = saved === 'open' ? true : saved === 'closed' ? false : button.dataset.ovDefaultOpen === 'true';
      var body = document.getElementById(button.getAttribute('aria-controls'));
      if (!body) return;
      body.hidden = !open;
      button.setAttribute('aria-expanded', open ? 'true' : 'false');
      button.setAttribute('aria-label', (open ? 'Collapse ' : 'Expand ') + button.closest('.stp-group').querySelector('.stp-group__t').textContent.trim());
    });
  }

  function refresh() { syncPills(); syncMenus(); apply(); syncAttn(); initEditors(); refreshOverviewFolds(); }

  document.addEventListener('click', function (e) {
    var button = e.target.closest && e.target.closest('[data-ov-fold]');
    if (!button) return;
    var open = button.getAttribute('aria-expanded') !== 'true';
    var body = document.getElementById(button.getAttribute('aria-controls'));
    if (!body) return;
    body.hidden = !open;
    button.setAttribute('aria-expanded', open ? 'true' : 'false');
    button.setAttribute('aria-label', (open ? 'Collapse ' : 'Expand ') + button.closest('.stp-group').querySelector('.stp-group__t').textContent.trim());
    try { localStorage.setItem(ovFoldPrefix + button.dataset.ovFold, open ? 'open' : 'closed'); } catch (err) {}
  });

  document.addEventListener('DOMContentLoaded', function () {
    var params = new URLSearchParams(window.location.search);
    var want = params.get('state');
    if (want && FILTERS.indexOf(want) !== -1) filter = want;
    tag = params.get('tag') || '';
    if (SORTS.indexOf(params.get('sort')) !== -1) sort = params.get('sort');
    refresh();
    syncURL();
    showActiveNav();

    // A source named by address (?feed=, which the server renders open, or
    // #src-{id}) is brought into view the way a reveal does it.
    var open = document.querySelector('#srcBody .mg-item:not(.mg-item--add) > .mg-ed');
    var hash = /^#src-([\w-]+)$/.exec(window.location.hash);
    var id = open ? open.closest('.mg-item').id.slice(4) : (hash && hash[1]);
    // /sources/manage?add=1 opens the add form.
    var add = document.querySelector('[data-src-add]');
    var pickIDs = params.get('select');
    setTimeout(function () {
      if (pickIDs) selectFromURL(pickIDs);
      else if (id && id !== 'new') reveal(id);
      else if (params.get('add') === '1' && add) add.click();
    }, 0);
  });
  document.body.addEventListener('htmx:afterSwap', function (e) {
    refresh();
    syncPlan();
    // A swap that took the status line away leaves no search to time.
    if (!document.querySelector('[data-src-status]')) clearTimeout(findTimer);
    var into = e.detail && e.detail.target;
    if (into && into.matches && into.matches('[data-src-cands]')) foundFeeds(into.closest('.mg-ed'));
  });
  // A live refresh swaps rows in out of band; the filters and search apply to
  // them the same way.
  document.body.addEventListener('htmx:oobAfterSwap', refresh);
})();

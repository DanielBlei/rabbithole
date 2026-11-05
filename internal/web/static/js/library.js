// The Sources Library page: instant client-side filtering of the list, the
// ticks on entries, and the bar that acts on them: Add previews the picked
// entries, Hide and Unhide change what the Library shows.
//
// Client-side because the whole Library is already on the page, a few hundred
// rows at most, the same call the Manage search makes. The filter box, the
// status and tag filters sit outside #libBody, so an action that repaints
// the list leaves them alone; this file re-applies the filter and the ticks
// after every swap. The ticked keys are kept here, not in the rows, so they
// survive those swaps; the bar carries them to the server as hidden fields,
// and the server builds the plan from its own copy of the Library. The plan
// replaces the list in #libBody and the list stays in the page, hidden:
// Cancel and "Back to the Library" bring it back without a request. The plan's
// own ticks and label are sources.js's.
//
// Hidden entries stay in the page, flagged data-hidden, and show only in the
// Hidden view. A section whose entries are all hidden drops to the bottom,
// dimmed, until its count is opened.
//
// Loaded on every page by the layout. Every lookup is guarded, so it costs
// nothing on a page where none of this markup exists.
(function () {
  var sel = {}; // key -> true, the entries ticked for the bar
  var labels = Object.create(null); // selected tag names
  var state = 'all';
  var libFoldPrefix = 'rh.sources.library.fold.';

  function page() { return document.querySelector('[data-lib]'); }
  function input() { return document.querySelector('[data-lib-filter]'); }
  function query() {
    var el = input();
    return el ? el.value : '';
  }
  function rows() { return Array.prototype.slice.call(document.querySelectorAll('#libBody .li-row')); }
  function planOpen() { return !!document.querySelector('#libBody [data-plan]'); }
  function isHidden(row) { return row.dataset.hidden === '1'; }

  // The Hidden view shows only hidden rows; every other view leaves them out.
  function inView(row) { return state === 'hidden' ? isHidden(row) : !isHidden(row); }

  // Search words narrow the selected status and tags.
  function parse() {
    return query().split(/\s+/).filter(function (w) { return !!w; });
  }

  function labelsOf(row) {
    return Array.prototype.map.call(row.querySelectorAll('[data-lib-chip]'), function (c) { return c.dataset.libChip; });
  }

  function matches(row, pq) {
    if (!inView(row)) return false;
    if (state !== 'all' && state !== 'hidden' && row.dataset.state !== state) return false;
    var selected = Object.keys(labels);
    if (selected.length && !selected.some(function (name) { return labelsOf(row).indexOf(name) !== -1; })) return false;
    var hay = row.dataset.hay || '';
    return pq.every(function (w) { return hay.indexOf(w.toLowerCase()) !== -1; });
  }

  // A section whose entries are all hidden stays in the page as a dimmed line
  // while nothing narrows the list.
  function goneSection(sec, pq) {
    return sec.hasAttribute('data-lib-sec-gone') && state === 'all' && !pq.length && !Object.keys(labels).length;
  }

  // What a filter that shows nothing says, worded for what was asked.
  function emptyText(pq, addableAll, scope, inScope) {
    var noTags = Object.keys(labels).length === 0;
    if (!inScope && state === 'hidden') return 'Nothing is hidden in ' + scope + '.';
    if (!inScope && state === 'all' && !pq.length && noTags) return 'Everything in ' + scope + ' is hidden.';
    if (!pq.length && noTags && state === 'add' && !addableAll) return 'You have everything in ' + scope + '.';
    if (!pq.length && noTags && state === 'removed') return 'You have not removed anything from ' + scope + '.';
    if (!pq.length && noTags && state === 'subscribed') return 'You have not added anything from ' + scope + ' yet.';
    return 'Nothing in ' + scope + ' matches that.';
  }

  function render() {
    var root = page();
    if (!root) return;
    var pq = parse();
    var all = rows();

    // A tick lives while its row is on the page.
    var present = {};
    all.forEach(function (row) { present[row.dataset.key] = true; });
    Object.keys(sel).forEach(function (key) { if (!present[key]) delete sel[key]; });

    var shown = 0, addableAll = 0, inScope = 0;
    var stateCounts = { all: 0, subscribed: 0, removed: 0, add: 0, hidden: 0 };
    all.forEach(function (row) {
      if (isHidden(row)) {
        stateCounts.hidden++;
      } else {
        stateCounts.all++;
        if (Object.prototype.hasOwnProperty.call(stateCounts, row.dataset.state)) stateCounts[row.dataset.state]++;
      }
      var hit = matches(row, pq);
      row.hidden = !hit;
      if (hit) shown++;
      if (inView(row)) inScope++;
      if (inView(row) && row.dataset.state === 'add') addableAll++;
      var box = row.querySelector('[data-lib-pick]');
      if (box) box.checked = !!sel[row.dataset.key];
    });

    document.querySelectorAll('#libBody [data-lib-sec]').forEach(function (sec) {
      var visible = Array.prototype.filter.call(sec.querySelectorAll('.li-row'), function (r) { return !r.hidden; });
      var gone = goneSection(sec, pq);
      sec.classList.toggle('li-sec--gone', gone);
      sec.hidden = visible.length === 0 && !gone;
      var n = sec.querySelector('[data-lib-sec-count]');
      if (n) {
        n.textContent = String(visible.length);
        n.hidden = gone;
      }
      var note = sec.querySelector('[data-lib-show-hidden]');
      if (note) note.hidden = !gone;
      var btn = sec.querySelector('[data-lib-sec-select]');
      if (!btn) return;
      var addable = visible.filter(function (r) { return r.dataset.state === 'add'; });
      btn.hidden = state === 'hidden' || addable.length < 2;
      if (btn.hidden) return;
      var every = addable.every(function (r) { return sel[r.dataset.key]; });
      var title = sec.dataset.title;
      btn.textContent = every ? 'Clear ' + addable.length : 'Select ' + addable.length + ' new';
      btn.setAttribute('aria-label', every
        ? 'Clear the ' + addable.length + ' selected in ' + title
        : 'Select the ' + addable.length + ' new entries in ' + title);
    });

    // Select shown ticks every row the view lets through, add or hide alike.
    var selectAll = document.querySelector('[data-lib-select-shown]');
    if (selectAll) {
      var shownRows = all.filter(function (r) { return !r.hidden; });
      selectAll.hidden = shownRows.length === 0;
      var allTicked = shownRows.length > 0 && shownRows.every(function (r) { return sel[r.dataset.key]; });
      selectAll.textContent = allTicked ? 'Clear ' + shownRows.length : 'Select all ' + shownRows.length;
      selectAll.setAttribute('aria-label', allTicked
        ? 'Clear the ' + shownRows.length + ' shown entries'
        : 'Select the ' + shownRows.length + ' shown entries');
    }

    document.querySelectorAll('[data-lib-label],[data-lib-chip]').forEach(function (b) {
      var name = b.dataset.libLabel || b.dataset.libChip;
      var on = !!labels[name];
      b.classList.toggle('is-on', on);
      b.setAttribute('aria-pressed', on ? 'true' : 'false');
    });
    // The status menu follows the state. Its option labels carry the counts and
    // the button keeps the plain name (data-label).
    var viewRadios = document.querySelectorAll('input[type="radio"][name="libfilter"]');
    Array.prototype.forEach.call(viewRadios, function (r) {
      if (r.value === state && !r.checked) {
        r.checked = true;
        r.dispatchEvent(new Event('change', {bubbles: true}));
      }
    });
    var view = document.getElementById('libViewMenu');
    if (view) {
      Array.prototype.forEach.call(view.querySelectorAll('[role="option"]'), function (o) {
        var n = o.querySelector('.stp-menu__n');
        if (n) n.textContent = o.dataset.label + ' (' + (stateCounts[o.dataset.value] || 0) + ')';
      });
    }
    var selectedCount = document.querySelector('[data-lib-tags-selected-count]');
    if (selectedCount) {
      var nSelected = Object.keys(labels).length;
      selectedCount.textContent = nSelected ? nSelected + ' selected' : '';
      selectedCount.hidden = !nSelected;
      var toggle = document.querySelector('[data-lib-tags-toggle]');
      if (toggle) {
        toggle.classList.toggle('is-on', nSelected > 0);
        toggle.setAttribute('aria-label', nSelected
          ? 'Filter by tags, ' + nSelected + ' selected'
          : 'Filter by tags');
      }
    }

    var x = document.querySelector('[data-lib-filter-clear]');
    if (x) x.hidden = !query();
    var count = document.querySelector('[data-lib-count]');
    if (count) count.textContent = shown === inScope ? '' : shown + ' of ' + inScope;

    var scope = root.dataset.scope || 'the Library';
    var done = document.querySelector('[data-lib-done]');
    if (done) done.hidden = !(inScope && !addableAll && state === 'all' && !pq.length && !Object.keys(labels).length);
    var empty = document.querySelector('[data-lib-empty]');
    if (empty) {
      empty.hidden = !(all.length && !shown);
      var t = empty.querySelector('[data-lib-empty-text]');
      if (t && !empty.hidden) t.textContent = emptyText(pq, addableAll, scope, inScope > 0);
    }
  }

  // The bar names the ticked entries. Each action form carries the picks it
  // acts on: Add the new ones, Hide the shown ones, Unhide the hidden ones.
  function renderBar() {
    var bar = document.querySelector('[data-lib-bar]');
    if (!bar) return;
    var keys = Object.keys(sel);
    var byKey = {};
    rows().forEach(function (r) { byKey[r.dataset.key] = r; });
    var addKeys = [], hideKeys = [], unhideKeys = [];
    keys.forEach(function (key) {
      var row = byKey[key];
      if (!row) return;
      if (isHidden(row)) {
        unhideKeys.push(key);
      } else {
        hideKeys.push(key);
        if (row.dataset.state === 'add') addKeys.push(key);
      }
    });
    bar.hidden = planOpen() || keys.length === 0;
    var text = bar.querySelector('[data-lib-bar-text]');
    if (text) text.textContent = keys.length + ' selected';

    function action(sel, picks, label, aria) {
      var form = bar.querySelector(sel);
      if (!form) return;
      form.hidden = picks.length === 0;
      form.querySelectorAll('input[name="pick"]').forEach(function (i) { i.remove(); });
      picks.forEach(function (key) {
        var i = document.createElement('input');
        i.type = 'hidden';
        i.name = 'pick';
        i.value = key;
        form.appendChild(i);
      });
      var b = form.querySelector('button');
      if (b) {
        b.textContent = label + ' ' + picks.length;
        b.setAttribute('aria-label', aria(picks.length));
      }
    }
    action('[data-lib-bar-add-form]', addKeys, 'Add', function (n) {
      return 'Add ' + n + ' selected ' + (n === 1 ? 'entry' : 'entries');
    });
    action('[data-lib-bar-hide-form]', hideKeys, 'Hide', function (n) {
      return 'Hide ' + n + ' selected ' + (n === 1 ? 'entry' : 'entries') + ' from the Library';
    });
    action('[data-lib-bar-unhide-form]', unhideKeys, 'Unhide', function (n) {
      return 'Unhide ' + n + ' selected ' + (n === 1 ? 'entry' : 'entries');
    });
  }

  // Per-section fold state, mirroring sources.js's Overview folds but under
  // their own data attributes and localStorage key so the two scripts never
  // fight over the same section.
  function refreshLibraryFolds() {
    document.querySelectorAll('[data-lib-fold]').forEach(function (button) {
      var key = libFoldPrefix + button.dataset.libFold;
      var saved = null;
      try { saved = localStorage.getItem(key); } catch (e) {}
      var open = saved === 'open' ? true : saved === 'closed' ? false : button.dataset.libFoldDefaultOpen === 'true';
      var body = document.getElementById(button.getAttribute('aria-controls'));
      if (!body) return;
      body.hidden = !open;
      button.setAttribute('aria-expanded', open ? 'true' : 'false');
      var title = button.closest('.li-sec__h').querySelector('.li-sec__t h3').textContent.trim();
      button.setAttribute('aria-label', (open ? 'Collapse ' : 'Expand ') + title);
    });
  }

  // Opens a section's fold (if closed) and persists that choice, used when
  // jumping to it from the sidebar or a #lib-sec-* link.
  function openLibraryFold(name) {
    var button = document.querySelector('[data-lib-fold="' + name + '"]');
    if (!button) return;
    var body = document.getElementById(button.getAttribute('aria-controls'));
    if (!body) return;
    body.hidden = false;
    button.setAttribute('aria-expanded', 'true');
    try { localStorage.setItem(libFoldPrefix + name, 'open'); } catch (e) {}
  }

  // Scrolls to and focuses the section a #lib-sec-<name> hash names, opening
  // its fold first so the jump always lands somewhere visible.
  function jumpToHash(hash) {
    if (!hash || hash.indexOf('#lib-sec-') !== 0) return;
    var name = hash.slice('#lib-sec-'.length);
    var section = document.getElementById('lib-sec-' + name);
    if (!section) return;
    openLibraryFold(name);
    section.scrollIntoView({ block: 'start' });
    var heading = section.querySelector('.li-sec__t h3');
    if (heading) heading.focus({ preventScroll: true });
  }

  // While the plan is up the header and the tools step aside.
  function syncMode() {
    var plan = planOpen();
    var head = document.querySelector('[data-lib-head]');
    var tools = document.querySelector('[data-lib-tools]');
    if (head) head.hidden = plan;
    if (tools) tools.hidden = plan;
  }

  function refresh() {
    render();
    syncMode();
    renderBar();
    refreshLibraryFolds();
  }

  function closePlan() {
    var plan = document.querySelector('#libBody [data-lib-plan]');
    var list = document.querySelector('#libBody [data-lib-list]');
    if (plan) plan.replaceChildren();
    if (list) list.hidden = false;
    refresh();
  }

  // Keep the tag picker open while working in the combined search control;
  // dismiss it as soon as the user moves elsewhere on the page.
  document.addEventListener('click', function (e) {
    var box = document.querySelector('[data-lib-tags-box]');
    if (!box || box.hidden || (e.target.closest && e.target.closest('.li-search-wrap, [data-lib-tags-box]'))) return;
    box.hidden = true;
    var toggle = document.querySelector('[data-lib-tags-toggle]');
    if (toggle) toggle.setAttribute('aria-expanded', 'false');
  });

  function setText(value) {
    var el = input();
    if (el) el.value = value;
  }

  function toggleLabel(name) {
    if (labels[name]) delete labels[name]; else labels[name] = true;
    render();
  }

  function clearFilters() {
    labels = Object.create(null);
    state = 'all';
    setText('');
    render();
  }

  document.addEventListener('input', function (e) {
    if (e.target.matches && e.target.matches('[data-lib-filter]')) render();
  });

  document.addEventListener('change', function (e) {
    var t = e.target;
    if (t.matches && t.matches('input[type="radio"][name="libfilter"]')) {
      state = t.value || 'all';
      render();
      return;
    }
    if (!t.matches || !t.matches('[data-lib-pick]')) return;
    if (t.checked) sel[t.value] = true; else delete sel[t.value];
    render();
    renderBar();
  });

  document.addEventListener('click', function (e) {
    var t = e.target;
    if (!t.closest || !t.closest('[data-lib]')) return;
    var el;
    if ((el = t.closest('[data-lib-label],[data-lib-chip]'))) {
      toggleLabel(el.dataset.libLabel || el.dataset.libChip);
    } else if ((el = t.closest('[data-lib-tags-toggle]'))) {
      var box = document.querySelector('[data-lib-tags-box]');
      var expanded = el.getAttribute('aria-expanded') !== 'true';
      el.setAttribute('aria-expanded', expanded ? 'true' : 'false');
      if (box) box.hidden = !expanded;
    } else if (t.closest('[data-lib-filter-clear]')) {
      setText('');
      render();
      var f = input();
      if (f) f.focus();
    } else if (t.closest('[data-lib-reset]')) {
      clearFilters();
    } else if (t.closest('[data-lib-show-hidden]')) {
      state = 'hidden';
      render();
      renderBar();
    } else if ((el = t.closest('[data-lib-select-shown]'))) {
      var shownRows = rows().filter(function (r) { return !r.hidden; });
      var every = shownRows.every(function (r) { return sel[r.dataset.key]; });
      shownRows.forEach(function (r) { if (every) delete sel[r.dataset.key]; else sel[r.dataset.key] = true; });
      render();
      renderBar();
    } else if ((el = t.closest('[data-lib-sec-select]'))) {
      var rowsOf = Array.prototype.filter.call(el.closest('[data-lib-sec]').querySelectorAll('.li-row'), function (r) {
        return !r.hidden && r.dataset.state === 'add';
      });
      var all = rowsOf.every(function (r) { return sel[r.dataset.key]; });
      rowsOf.forEach(function (r) { if (all) delete sel[r.dataset.key]; else sel[r.dataset.key] = true; });
      render();
      renderBar();
    } else if (t.closest('[data-lib-bar-clear]')) {
      sel = {};
      render();
      renderBar();
    } else if (t.closest('[data-plan-cancel]')) {
      closePlan();
    } else if ((el = t.closest('[data-lib-fold]'))) {
      var open = el.getAttribute('aria-expanded') !== 'true';
      var body = document.getElementById(el.getAttribute('aria-controls'));
      if (!body) return;
      body.hidden = !open;
      el.setAttribute('aria-expanded', open ? 'true' : 'false');
      var title = el.closest('.li-sec__h').querySelector('.li-sec__t h3').textContent.trim();
      el.setAttribute('aria-label', (open ? 'Collapse ' : 'Expand ') + title);
      try { localStorage.setItem(libFoldPrefix + el.dataset.libFold, open ? 'open' : 'closed'); } catch (err) {}
    } else if ((el = t.closest('[data-lib-nav-anchor]')) && page()) {
      // Already on the Library page: scroll in place instead of a full reload.
      e.preventDefault();
      var hash = '#' + el.dataset.libNavAnchor;
      history.pushState(null, '', hash);
      jumpToHash(hash);
    }
  });

  window.addEventListener('hashchange', function () { jumpToHash(location.hash); });
  document.addEventListener('DOMContentLoaded', function () { jumpToHash(location.hash); });

  // Esc in the filter box clears every filter, like "Clear filters". An open modal keeps the
  // key. Bound ahead of modal.js, like sources.js, so the key is spent here first.
  document.addEventListener('keydown', function (e) {
    if (e.key !== 'Escape' || e.defaultPrevented) return;
    if (document.documentElement.classList.contains('modal-open')) return;
    var field = e.target.closest && e.target.closest('[data-lib-filter]');
    if (!field || !(field.value || Object.keys(labels).length || state !== 'all')) return;
    e.preventDefault();
    e.stopImmediatePropagation();
    field.value = '';
    labels = Object.create(null);
    state = 'all';
    render();
  });

  document.addEventListener('DOMContentLoaded', refresh);
  // A request that changes the Library starts from a clean set of ticks: the
  // entries it acted on are no longer where the ticks were.
  document.body.addEventListener('htmx:beforeRequest', function (e) {
    var path = e.detail && e.detail.requestConfig && e.detail.requestConfig.path;
    if (path && /\/sources\/library\/(apply|hide|unhide|restore)$/.test(path)) sel = {};
  });
  document.body.addEventListener('htmx:afterSwap', function () {
    var root = page();
    if (!root) return;
    refresh();
    if (planOpen()) {
      var back = root.querySelector('.li-back');
      if (back) back.focus({ preventScroll: true });
      root.scrollIntoView({ block: 'nearest' });
    }
  });
})();

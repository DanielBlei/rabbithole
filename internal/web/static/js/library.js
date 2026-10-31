// The Sources Library page: instant client-side filtering of the list, the
// ticks on entries that can be added, and the Add bar that previews them.
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
// Loaded on every page by the layout. Every lookup is guarded, so it costs
// nothing on a page where none of this markup exists.
(function () {
  var sel = {}; // key -> true, the entries ticked for Add N
  var labels = Object.create(null); // selected tag names
  var state = 'all';

  function page() { return document.querySelector('[data-lib]'); }
  function input() { return document.querySelector('[data-lib-filter]'); }
  function query() {
    var el = input();
    return el ? el.value : '';
  }
  function rows() { return Array.prototype.slice.call(document.querySelectorAll('#libBody .li-row')); }
  function planOpen() { return !!document.querySelector('#libBody [data-plan]'); }

  // Search words narrow the selected status and tags.
  function parse() {
    return query().split(/\s+/).filter(function (w) { return !!w; });
  }

  function labelsOf(row) {
    return Array.prototype.map.call(row.querySelectorAll('[data-lib-chip]'), function (c) { return c.dataset.libChip; });
  }

  function matches(row, pq) {
    if (state !== 'all' && row.dataset.state !== state) return false;
    var selected = Object.keys(labels);
    if (selected.length && !selected.some(function (name) { return labelsOf(row).indexOf(name) !== -1; })) return false;
    var hay = row.dataset.hay || '';
    return pq.every(function (w) { return hay.indexOf(w.toLowerCase()) !== -1; });
  }

  // What a filter that shows nothing says, worded for what was asked.
  function emptyText(pq, addableAll, scope) {
    var noTags = Object.keys(labels).length === 0;
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

    // A tick lives while its row can still be added.
    var present = {};
    all.forEach(function (row) { if (row.dataset.state === 'add') present[row.dataset.key] = true; });
    Object.keys(sel).forEach(function (key) { if (!present[key]) delete sel[key]; });

    var shown = 0, addableAll = 0;
    var stateCounts = { all: all.length, subscribed: 0, removed: 0, add: 0 };
    all.forEach(function (row) {
      var hit = matches(row, pq);
      row.hidden = !hit;
      if (hit) shown++;
      if (Object.prototype.hasOwnProperty.call(stateCounts, row.dataset.state)) stateCounts[row.dataset.state]++;
      if (row.dataset.state === 'add') {
        addableAll++;
        var box = row.querySelector('[data-lib-pick]');
        if (box) box.checked = !!sel[row.dataset.key];
      }
    });

    document.querySelectorAll('#libBody [data-lib-sec]').forEach(function (sec) {
      var visible = Array.prototype.filter.call(sec.querySelectorAll('.li-row'), function (r) { return !r.hidden; });
      sec.hidden = visible.length === 0;
      var n = sec.querySelector('[data-lib-sec-count]');
      if (n) n.textContent = String(visible.length);
      var btn = sec.querySelector('[data-lib-sec-select]');
      if (!btn) return;
      var addable = visible.filter(function (r) { return r.dataset.state === 'add'; });
      btn.hidden = addable.length < 2;
      if (btn.hidden) return;
      var every = addable.every(function (r) { return sel[r.dataset.key]; });
      var title = sec.dataset.title;
      btn.textContent = every ? 'Clear ' + addable.length : 'Select ' + addable.length + ' new';
      btn.setAttribute('aria-label', every
        ? 'Clear the ' + addable.length + ' selected in ' + title
        : 'Select the ' + addable.length + ' new entries in ' + title);
    });

    document.querySelectorAll('[data-lib-label],[data-lib-chip]').forEach(function (b) {
      var name = b.dataset.libLabel || b.dataset.libChip;
      var on = !!labels[name];
      b.classList.toggle('is-on', on);
      b.setAttribute('aria-pressed', on ? 'true' : 'false');
    });
    document.querySelectorAll('[data-lib-state]').forEach(function (b) { b.checked = b.dataset.libState === state; });
    document.querySelectorAll('[data-lib-state-count]').forEach(function (el) {
      el.textContent = String(stateCounts[el.dataset.libStateCount] || 0);
    });
    var selectedCount = document.querySelector('[data-lib-tags-selected-count]');
    if (selectedCount) {
      var nSelected = Object.keys(labels).length;
      selectedCount.textContent = nSelected ? nSelected + ' selected' : '';
      selectedCount.hidden = !nSelected;
    }

    var x = document.querySelector('[data-lib-filter-clear]');
    if (x) x.hidden = !query();
    var count = document.querySelector('[data-lib-count]');
    if (count) count.textContent = shown === all.length ? '' : shown + ' of ' + all.length;

    var scope = root.dataset.scope || 'the Library';
    var done = document.querySelector('[data-lib-done]');
    if (done) done.hidden = !(all.length && !addableAll && state === 'all' && !pq.length && !Object.keys(labels).length);
    var empty = document.querySelector('[data-lib-empty]');
    if (empty) {
      empty.hidden = !(all.length && !shown);
      var t = empty.querySelector('[data-lib-empty-text]');
      if (t && !empty.hidden) t.textContent = emptyText(pq, addableAll, scope);
    }
  }

  // The bar names the ticked entries and carries their keys to the server.
  function renderBar() {
    var bar = document.querySelector('[data-lib-bar]');
    if (!bar) return;
    var keys = Object.keys(sel);
    bar.hidden = planOpen() || keys.length === 0;
    var text = bar.querySelector('[data-lib-bar-text]');
    if (text) text.textContent = keys.length + ' selected';
    var add = bar.querySelector('[data-lib-bar-add]');
    if (add) {
      add.textContent = 'Add ' + keys.length;
      add.setAttribute('aria-label', 'Add ' + keys.length + ' selected ' + (keys.length === 1 ? 'entry' : 'entries'));
    }
    bar.querySelectorAll('input[name="pick"]').forEach(function (i) { i.remove(); });
    keys.forEach(function (key) {
      var i = document.createElement('input');
      i.type = 'hidden';
      i.name = 'pick';
      i.value = key;
      bar.appendChild(i);
    });
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
  }

  function closePlan() {
    var plan = document.querySelector('#libBody [data-lib-plan]');
    var list = document.querySelector('#libBody [data-lib-list]');
    if (plan) plan.replaceChildren();
    if (list) list.hidden = false;
    refresh();
  }

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
    if (t.matches && t.matches('[data-lib-state]')) {
      state = t.dataset.libState || 'all';
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
      var label = el.querySelector('[data-lib-tags-state]');
      if (label) label.textContent = expanded ? 'Hide' : 'Show';
    } else if (t.closest('[data-lib-filter-clear]')) {
      setText('');
      render();
      var f = input();
      if (f) f.focus();
    } else if (t.closest('[data-lib-reset]')) {
      clearFilters();
    } else if ((el = t.closest('[data-lib-sec-select]'))) {
      var rowsOf = Array.prototype.filter.call(el.closest('[data-lib-sec]').querySelectorAll('.li-row'), function (r) {
        return !r.hidden && r.dataset.state === 'add';
      });
      var every = rowsOf.every(function (r) { return sel[r.dataset.key]; });
      rowsOf.forEach(function (r) { if (every) delete sel[r.dataset.key]; else sel[r.dataset.key] = true; });
      render();
      renderBar();
    } else if (t.closest('[data-lib-bar-clear]')) {
      sel = {};
      render();
      renderBar();
    } else if (t.closest('[data-plan-cancel]')) {
      closePlan();
    }
  });

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

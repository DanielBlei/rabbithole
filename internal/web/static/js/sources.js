// The Sources manage page: instant client-side search and filtering of the
// source list, the inline editors (dirty state, discard guard, live hints,
// reveal), the attention block's collapse, the copy button on the export
// modal, and importing a feeds file by picker or drop.
//
// Client-side because the whole set is already on the page and a personal
// source list is tens of rows, not thousands, the same call the Feed page's
// filters make. The search box and filter pills live outside #srcBody, so an
// edit that repaints the list leaves the query intact; this file re-applies it
// after every swap, including the one that first shows the list. The pill
// counts, summary line and attention block are re-sent by the server after a
// mutation; their state (which pill is checked, whether the block is open) is
// kept here so it survives those swaps. An editor lives in its source's unit
// (#src-{id}), which the server swaps alone; closing one is done here by
// removing it, so no request is made.
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
  var filter = 'all';
  var attnOpen = null; // decided the first time the block is on screen
  var scrollOnOpen = false; // the next editor to arrive is one the user asked for
  var focusField = null; // 'url' when Edit URL asked for the address
  var flashAfter = null; // unit to flash once its swap lands
  var refocusForm = null; // the editor whose form was just submitted

  function input() { return document.querySelector('[data-src-query]'); }
  function query() {
    var el = input();
    return el ? el.value.trim().toLowerCase() : '';
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

  function apply() {
    var q = query();
    var rows = document.querySelectorAll('[data-src-row]');
    var shown = 0, live = 0, removedN = 0;
    rows.forEach(function (row) {
      if (row.dataset.removed === '1') removedN++; else live++;
      // A source being edited with unsaved changes stays in view, whatever the
      // search says; the pills ask before hiding it (see the change handler).
      var form = row.querySelector(':scope > .mg-ed');
      var hit = (form && isDirty(form)) || (inFilter(row, filter) && matches(row, q));
      row.hidden = !hit;
      if (hit) shown++;
    });

    var toolbar = document.querySelector('[data-src-toolbar]');
    if (toolbar) toolbar.hidden = rows.length === 0;
    var x = document.querySelector('.mg-search__x');
    if (x) x.hidden = !q;

    var count = document.querySelector('[data-src-matches]');
    if (count) {
      count.hidden = !q;
      count.textContent = shown + ' of ' + (filter === 'removed' ? removedN : live);
    }

    // With no rows at all the server has drawn the first-run state itself.
    if (!rows.length) return;
    if (shown) showEmpty(null);
    else if (q) showEmpty('search');
    else if (filter === 'all' && !live) showEmpty('first');
    else showEmpty('filter', EMPTY[filter]);
  }

  function syncPills() {
    document.querySelectorAll('[data-src-state]').forEach(function (r) { r.checked = r.value === filter; });
  }

  // The pill is the state; the address carries it so a link or a reload lands
  // on the same view.
  function syncURL() {
    try {
      var u = new URL(window.location.href);
      if (filter === 'all') u.searchParams.delete('state'); else u.searchParams.set('state', filter);
      history.replaceState(null, '', u.pathname + u.search + u.hash);
    } catch (e) { /* not fatal */ }
  }

  function setFilter(f) {
    filter = FILTERS.indexOf(f) === -1 ? 'all' : f;
    syncPills();
    apply();
    syncURL();
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
  }

  function focusEditor(form, which) {
    var field = form.querySelector(which === 'url' ? '[name="url"]' : '[name="name"]');
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
    if (isAddForm(form)) { unit.remove(); return; }
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
        focusEditor(form, scrollOnOpen ? focusField : null);
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
    if (!t.closest) return;

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
    if (t.closest('[data-src-showall]')) { setFilter('all'); return; }

    // Anything that would replace or close a dirty editor asks first: opening
    // another source, adding, or a switch on the source being edited. Controls
    // inside the editor, and in dialogs, are their own business.
    var act = t.closest('[data-src-open],[data-src-add],[data-src-repaint],[data-src-import],[hx-target]');
    if (!act || act.closest('.mg-ed') || act.closest('#modal,#modalTop')) return;
    var opens = act.matches('[data-src-open]');
    var target = act.getAttribute('hx-target') || '';
    var unit = /^#src-[\w-]+$/.test(target) ? document.querySelector(target) : null;
    var own = unit && unit.querySelector(':scope > .mg-ed');
    var dirty = dirtyEditor();
    var replaces = opens || target === '#srcBody' || (own && own === dirty) ||
      act.matches('[data-src-add],[data-src-repaint],[data-src-import]');
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
    // A switch pressed while its editor is open keeps the editor open.
    if (target && /\/enabled$/.test(path) && target.querySelector(':scope > .mg-ed')) e.detail.parameters.open = '1';
    // A live refresh leaves out the rows being edited.
    if (/^\/sources\/live\b/.test(path)) {
      var ids = [];
      document.querySelectorAll('#srcBody .mg-item:not(.mg-item--add) > .mg-ed').forEach(function (f) {
        ids.push(f.closest('.mg-item').id.slice(4));
      });
      e.detail.parameters.open = ids.join(',');
    }
  });

  document.body.addEventListener('htmx:beforeRequest', function (e) {
    var elt = e.detail.elt;
    var form = elt && elt.closest && elt.closest('.mg-ed');
    if (form) refocusForm = form.dataset.srcEd;
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
  // ahead of, so step aside), then the editor, then the search text. From the
  // search box the text goes first: that is where the key was pressed. Both
  // modal.js and this are bound on document, where stopPropagation does nothing
  // to a sibling listener, so this file is loaded ahead of modal.js to run first.
  document.addEventListener('keydown', function (e) {
    if (e.key !== 'Escape' || e.defaultPrevented) return;
    if (document.documentElement.classList.contains('modal-open')) return;
    var field = e.target.closest && e.target.closest('[data-src-query]');
    if (field && field.value) {
      e.stopImmediatePropagation();
      field.value = '';
      apply();
      return;
    }
    if (e.target.closest && e.target.closest('.tagput__suggest')) return;
    var form = (e.target.closest && e.target.closest('.mg-ed')) || document.querySelector('#srcBody .mg-ed');
    if (!form) return;
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

  // ---- import ----------------------------------------------------------
  // The server answers with the repainted #srcBody (and the out-of-band tally),
  // so the response is swapped in the way an htmx request would have.
  function sendFeeds(file) {
    var body = document.getElementById('srcBody');
    if (!file || !body) return;
    var form = new FormData();
    form.append('file', file);
    fetch('/sources/import', { method: 'POST', body: form, credentials: 'same-origin' })
      .then(function (res) {
        // An expired session redirects to the login page; let the gate take over
        // rather than swap that page into the editor.
        if (res.redirected || new URL(res.url).pathname !== '/sources/import') {
          window.location.reload();
          return null;
        }
        if (!res.ok) throw new Error('status ' + res.status);
        return res.text();
      })
      .then(function (html) {
        var target = document.getElementById('srcBody');
        if (html !== null && target) htmx.swap(target, html, { swapStyle: 'outerHTML' });
      })
      .catch(function () { importError('could not import the feeds file'); });
  }

  // Same markup as the server's error banner, put at the top of the list column.
  function importError(msg) {
    var col = document.querySelector('#srcBody .src__col');
    if (!col) return;
    var old = col.querySelector('.src__import');
    if (old) old.remove();
    var el = document.createElement('div');
    el.className = 'sdet__err src__import';
    el.setAttribute('role', 'alert');
    el.textContent = msg;
    col.insertBefore(el, col.firstChild);
    var zone = document.getElementById('srcBody');
    if (zone) zone.classList.remove('is-drop');
  }

  document.addEventListener('click', function (e) {
    var btn = e.target.closest('[data-src-import]');
    if (!btn) return;
    var input = document.querySelector('[data-src-file]');
    if (input) input.click();
  });

  document.addEventListener('change', function (e) {
    if (!e.target.matches('[data-src-file]')) return;
    sendFeeds(e.target.files[0]);
    e.target.value = '';
  });

  function dropTarget(e) {
    return e.target.closest && e.target.closest('[data-src-drop]');
  }
  function hasFiles(e) {
    return e.dataTransfer && Array.prototype.indexOf.call(e.dataTransfer.types || [], 'Files') !== -1;
  }

  document.addEventListener('dragover', function (e) {
    var zone = dropTarget(e);
    if (!zone || !hasFiles(e)) return;
    e.preventDefault();
    zone.classList.add('is-drop');
  });

  document.addEventListener('dragleave', function (e) {
    var zone = dropTarget(e);
    if (zone && !zone.contains(e.relatedTarget)) zone.classList.remove('is-drop');
  });

  document.addEventListener('drop', function (e) {
    var zone = dropTarget(e);
    if (!zone || !hasFiles(e)) return;
    e.preventDefault();
    zone.classList.remove('is-drop');
    // A drop replaces the whole list, so it asks first when an editor has
    // unsaved changes.
    var file = e.dataTransfer.files[0];
    guardThen(function () { sendFeeds(file); }, null);
  });

  // On narrow screens the section nav is a scrolling row. Bring the current
  // item into it; a column nav has no overflow to correct.
  function showActiveNav() {
    var el = document.querySelector('.sh-ni.is-on');
    var list = el && el.closest('.sh-nav__list');
    if (!list || getComputedStyle(list).flexDirection !== 'row') return;
    el.scrollIntoView({block: 'nearest', inline: 'nearest'});
  }

  function refresh() { syncPills(); apply(); syncAttn(); initEditors(); }

  document.addEventListener('DOMContentLoaded', function () {
    var params = new URLSearchParams(window.location.search);
    var want = params.get('state');
    if (want && FILTERS.indexOf(want) !== -1) filter = want;
    refresh();
    showActiveNav();

    // A source named by address (?feed=, which the server renders open, or
    // #src-{id}) is brought into view the way a reveal does it.
    var open = document.querySelector('#srcBody .mg-item:not(.mg-item--add) > .mg-ed');
    var hash = /^#src-([\w-]+)$/.exec(window.location.hash);
    var id = open ? open.closest('.mg-item').id.slice(4) : (hash && hash[1]);
    // /sources/manage?add=1 opens the add form.
    var add = document.querySelector('[data-src-add]');
    setTimeout(function () {
      if (id && id !== 'new') reveal(id);
      else if (params.get('add') === '1' && add) add.click();
    }, 0);
  });
  document.body.addEventListener('htmx:afterSwap', refresh);
  // A live refresh swaps rows in out of band; the filters and search apply to
  // them the same way.
  document.body.addEventListener('htmx:oobAfterSwap', refresh);
})();

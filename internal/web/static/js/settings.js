// The Settings page (templates/settings.html). Each tab is a pane of the same
// document, so switching is show/hide plus a history entry: the tab bar is
// plain links, which still work if this never runs. Owns the tab switching,
// the search across every pane, the feeds import button on General, and the
// editing of the config form (General, Scoring, Advanced): one draft across the
// three tabs, one save bar, a review dialog, test connection, pasting a key,
// rollback, import, and the guard for leaving with unsaved edits.
(function(){
  var root = document.querySelector('[data-stp]');
  if (!root) return;
  var panes = Array.prototype.slice.call(root.querySelectorAll('[data-stp-pane]'));
  var tabs = Array.prototype.slice.call(root.querySelectorAll('[data-stp-tab]'));
  var form = root.querySelector('[data-stp-form]');
  var search = root.querySelector('[data-stp-search]');
  var none = root.querySelector('[data-stp-nomatch]');
  var cmdTab = root.querySelector('[data-stp-cmdtab]');
  var current = (panes.filter(function(p){ return !p.hidden; })[0] || panes[0]).dataset.stpPane;

  // ---- tabs ------------------------------------------------------------
  // The bar's thumb sits under the selected tab: its offset and width are
  // written as custom properties. The first placement skips the slide.
  var tabBar = root.querySelector('.stp-tabs');
  function placeThumb(){
    var on = tabs.filter(function(t){ return t.classList.contains('is-on'); })[0];
    if (!tabBar || !on) return;
    tabBar.style.setProperty('--thumb-x', on.offsetLeft + 'px');
    tabBar.style.setProperty('--thumb-w', on.offsetWidth + 'px');
  }
  placeThumb();
  requestAnimationFrame(function(){ if (tabBar) tabBar.classList.add('is-set'); });
  window.addEventListener('resize', placeThumb);
  if (document.fonts && document.fonts.ready) document.fonts.ready.then(placeThumb);
  if (tabBar) tabBar.addEventListener('keydown', function(e){
    var i = tabs.indexOf(document.activeElement), n = tabs.length, to;
    if (i < 0) return;
    if (e.key === 'ArrowRight') to = (i + 1) % n;
    else if (e.key === 'ArrowLeft') to = (i - 1 + n) % n;
    else if (e.key === 'Home') to = 0;
    else if (e.key === 'End') to = n - 1;
    else return;
    e.preventDefault();
    tabs[to].focus();
    tabs[to].click();
  });
  tabs.forEach(function(t){ if (t.classList.contains('is-on') && t.scrollIntoView) t.scrollIntoView({inline: 'center', block: 'nearest'}); });
  // Arrows in the page margins step to the neighbouring tab. Each shows only
  // when its margin is wide enough to hold it, and not past either end.
  var steps = Array.prototype.slice.call(root.querySelectorAll('[data-stp-step]'));
  var pane = root.querySelector('.stp.pane');
  function placeSteps(){
    var i = tabs.indexOf(tabs.filter(function(t){ return t.classList.contains('is-on'); })[0]);
    var r = pane && pane.getBoundingClientRect(), vw = document.documentElement.clientWidth, room = 52;
    steps.forEach(function(b){
      var d = +b.dataset.stpStep, to = i + d;
      var fits = r && (d < 0 ? r.left >= room : vw - r.right >= room);
      b.hidden = !(fits && to >= 0 && to < tabs.length);
      if (b.hidden) return;
      b.style.left = (d < 0 ? r.left - 44 : r.right + 12) + 'px';
      b.title = tabs[to].textContent;
    });
  }
  steps.forEach(function(b){
    b.addEventListener('click', function(){
      var i = tabs.indexOf(tabs.filter(function(t){ return t.classList.contains('is-on'); })[0]);
      if (tabs[i + +b.dataset.stpStep]) tabs[i + +b.dataset.stpStep].click();
    });
  });
  window.addEventListener('resize', placeSteps);
  function show(slug){
    if (!panes.some(function(p){ return p.dataset.stpPane === slug; })) return false;
    current = slug;
    if (slug !== 'advanced' && typeof setLocked === 'function') setLocked(true);
    panes.forEach(function(p){ p.hidden = p.dataset.stpPane !== slug; });
    tabs.forEach(function(t){
      var on = t.dataset.stpTab === slug;
      t.classList.toggle('is-on', on);
      if (on) t.setAttribute('aria-current', 'page'); else t.removeAttribute('aria-current');
    });
    var cur = tabs.filter(function(t){ return t.dataset.stpTab === slug; })[0];
    placeThumb();
    if (cmdTab) cmdTab.textContent = slug;
    if (cur && cur.scrollIntoView) cur.scrollIntoView({inline: 'center', block: 'nearest'});
    placeSteps();
    return true;
  }
  function slugOf(path){
    var m = /^\/settings\/([^/]+)\/?$/.exec(path);
    return m ? m[1] : null;
  }
  root.addEventListener('click', function(e){
    var tab = e.target.closest('[data-stp-tab]');
    if (!tab || e.button !== 0 || e.ctrlKey || e.metaKey || e.shiftKey || e.altKey) return;
    var slug = tab.dataset.stpTab;
    e.preventDefault();
    if (search && search.value) { search.value = ''; filter(''); }
    if (slug !== current && show(slug)) history.pushState({stp: slug}, '', tab.getAttribute('href'));
    window.scrollTo(0, 0);
  });
  window.addEventListener('popstate', function(){
    var slug = slugOf(window.location.pathname);
    if (slug) show(slug);
  });
  history.replaceState({stp: current}, '');
  placeSteps();

  // ---- search ----------------------------------------------------------
  // Matches rows across every pane. While there is a query all the panes with
  // a match are shown, and clearing it puts the current tab back.
  function filter(q){
    q = q.trim().toLowerCase();
    root.classList.toggle('stp--searching', !!q);
    var any = false;
    panes.forEach(function(p){
      var hits = 0;
      p.querySelectorAll('.stp-row').forEach(function(row){
        var hit = !q || row.textContent.toLowerCase().indexOf(q) !== -1;
        row.hidden = !hit;
        if (hit && !row.classList.contains('is-off')) hits++;
      });
      p.querySelectorAll('.stp-group').forEach(function(g){
        g.hidden = !!q && !g.querySelector('.stp-row:not([hidden])') && !g.querySelector('.stp-free');
      });
      p.classList.toggle('is-empty', !!q && !hits);
      p.hidden = q ? !hits : p.dataset.stpPane !== current;
      if (hits) any = true;
    });
    if (none) none.hidden = !q || any;
  }
  if (search){
    search.addEventListener('input', function(){ filter(search.value); });
    search.addEventListener('keydown', function(e){
      if (e.key === 'Escape' && search.value){ search.value = ''; filter(''); e.stopPropagation(); }
    });
    document.addEventListener('keydown', function(e){
      if (e.key !== '/' || e.ctrlKey || e.metaKey || e.altKey) return;
      var t = e.target;
      if (t && (t.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName))) return;
      if (document.documentElement.classList.contains('modal-open')) return;
      e.preventDefault();
      search.focus();
    });
  }

  // ---- config form -----------------------------------------------------
  // Every field is a row with data-fld; its value lives in real inputs of the
  // one form, so the page can post the whole draft. The server rebuilds the
  // candidate config from it: blank means the default, and a value equal to the
  // default stays unpinned for a setting that was never set.
  var modal = document.getElementById('modal');
  var askLayer = document.getElementById('modalTop');
  var applied = root.querySelector('[data-stp-applied]');
  var banner = root.querySelector('[data-stp-banner]');
  var bar = root.querySelector('[data-stp-savebar]');
  var rows = form ? Array.prototype.slice.call(form.querySelectorAll('[data-fld]')) : [];
  var snap = {};
  var locked = true;
  var repl = root.querySelector('[data-stp-replies]');
  var skipGuard = false;
  var provider = function(){
    var r = form && form.querySelector('input[name="inference.provider"]:checked');
    return r ? r.value : 'ollama';
  };

  function ctl(row, sel){ return row.querySelector(sel); }
  function valueOf(row){
    var k = row.dataset.kind, name = row.dataset.fld;
    if (k === 'choice') { var c = row.querySelector('input[type=radio]:checked'); return (c ? c.value : '') + (isReset(row) ? '*' : ''); }
    if (k === 'sys') {
      var m = row.querySelector('input[type=radio]:checked'), p = row.querySelector('textarea');
      var mode = m ? m.value : 'default';
      // Custom text equal to the built-in is saved as unset, so it is not a change.
      if (mode === 'custom' && p.value.trim() === p.dataset.builtin.trim()) return 'default';
      return mode === 'custom' ? 'custom:' + p.value.trim() : mode;
    }
    var i = row.querySelector('input[name="' + name + '"]');
    return (i ? i.value.trim() : '') + (isReset(row) ? '*' : '');
  }
  // The mode the person picked, as opposed to valueOf, which folds a custom text
  // equal to the built-in into 'default' for the change count.
  function sysMode(row){
    var m = row.querySelector('input[type=radio]:checked');
    return m ? m.value : 'default';
  }
  function resetInput(row){ return row.querySelector('[data-stp-reset]'); }
  function isReset(row){ var r = resetInput(row); return !!r && r.value === '1'; }
  function setValue(row, v){
    var k = row.dataset.kind, name = row.dataset.fld;
    if (k === 'choice') {
      var cstar = v.slice(-1) === '*', cv = cstar ? v.slice(0, -1) : v, cr = resetInput(row);
      row.querySelectorAll('input[type=radio]').forEach(function(r){ r.checked = r.value === cv; });
      if (cr) cr.value = cstar ? '1' : '0';
    } else if (k === 'sys') {
      var mode = v.indexOf('custom:') === 0 ? 'custom' : v;
      row.querySelectorAll('input[type=radio]').forEach(function(r){ r.checked = r.value === mode; });
      row.querySelector('textarea').dataset.draft = mode === 'custom' ? v.slice(7) : '';
    } else {
      var star = v.slice(-1) === '*';
      var i = row.querySelector('input[name="' + name + '"]');
      if (i) i.value = star ? v.slice(0, -1) : v;
      var r = resetInput(row);
      if (r) r.value = star ? '1' : '0';
    }
    paint(row);
  }
  // Puts the control back on the default and tells the server to unpin it.
  // The default for the provider the form has picked: a few defaults differ per
  // provider, and data-defaults carries them all.
  function defaultOf(row){
    var m = row.dataset.defaults;
    if (m) { try { var d = JSON.parse(m)[provider()]; if (d !== undefined) return d; } catch (e) {} }
    return row.dataset.default;
  }
  function useDefault(row){
    var k = row.dataset.kind, d = defaultOf(row);
    if (k === 'choice') row.querySelectorAll('input[type=radio]').forEach(function(r){ r.checked = r.value === d; });
    else { var i = row.querySelector('input[name="' + row.dataset.fld + '"]'); if (i) i.value = d; }
    resetInput(row).value = '1';
    paint(row);
  }
  // Shows the control the way its value says: switch state, step number, the
  // instructions path field.
  function paint(row){
    var k = row.dataset.kind, v = valueOf(row).replace(/\*$/, '');
    var ud = ctl(row, '[data-stp-usedefault]');
    if (ud) ud.hidden = isReset(row) || row.dataset.raw === '';
    if (k === 'bool') {
      var sw = ctl(row, '[data-stp-switch]'), on = v === 'true';
      sw.classList.toggle('is-on', on);
      sw.setAttribute('aria-checked', on ? 'true' : 'false');
      sw.querySelector('.wx-sw__txt').textContent = on ? 'On' : 'Off';
    } else if (k === 'step') {
      var n = parseInt(v, 10), st = ctl(row, '.stp-step');
      ctl(row, '[data-stp-stepval]').textContent = v;
      ctl(row, '[data-stp-less]').disabled = n <= parseInt(st.dataset.min, 10);
      ctl(row, '[data-stp-more]').disabled = n >= parseInt(st.dataset.max, 10);
    } else if (k === 'sys') {
      var ta = row.querySelector('textarea'), mode = sysMode(row);
      row.dataset.mode = mode;
      var text = mode === 'custom' ? ta.dataset.draft : mode === 'default' ? ta.dataset.builtin : '';
      if (ta.value !== text) ta.value = text;
      ta.readOnly = locked || mode !== 'custom';
      ta.classList.toggle('is-none', mode === 'none');
    }
  }
  function isDefault(row){
    var v = valueOf(row), k = row.dataset.kind;
    if (k === 'key') return false;
    if (k === 'sys') return v === 'default' && row.dataset.raw === '';
    if (v.slice(-1) === '*') return true;
    if (k === 'text' || k === 'num') return v === '' || (v === defaultOf(row) && row.dataset.raw === '');
    return v === defaultOf(row) && row.dataset.raw === '';
  }
  function reads(el){
    var a = el.dataset.applies;
    return !a || a.split(' ').indexOf(provider()) >= 0;
  }

  // A field that was never saved is already on its default, so resetting it
  // changes nothing in the stored document.
  function isChanged(r){
    if (!reads(r)) return false;
    if (isReset(r) && r.dataset.raw === '') return false;
    return valueOf(r) !== snap[r.dataset.fld];
  }
  function changedRows(){ return rows.filter(isChanged); }
  function dirty(){ return form && !skipGuard && changedRows().length > 0; }

  function refresh(){
    if (!form) return;
    // Rows the provider does not read leave the page.
    form.querySelectorAll('[data-applies]').forEach(function(el){ el.classList.toggle('is-off', !reads(el)); });
    rows.forEach(function(r){
      var tag = ctl(r, '[data-stp-tag]'), mod = ctl(r, '[data-stp-mod]'), kc = ctl(r, '[data-stp-keychip]');
      var changed = isChanged(r);
      // An emptied reference reads as no key, whatever the saved one said. A key
      // pasted into .env whose reference is not saved yet says so.
      if (kc) {
        if (!kc.dataset.was) kc.dataset.was = kc.className + '|' + kc.textContent;
        var was = kc.dataset.was.split('|'), empty = r.querySelector('input[name="' + r.dataset.fld + '"]').value.trim() === '';
        var pending = r.dataset.pasted === '1' && changed && !empty;
        kc.className = pending ? 'stp-chip stp-chip--warn' : empty && r.dataset.raw !== '' ? 'stp-chip stp-chip--dim' : was[0];
        kc.textContent = pending ? 'In .env, not saved yet' : empty && r.dataset.raw !== '' ? 'No key set' : was[1];
        var note = ctl(r, '[data-stp-keynote]');
        if (note) note.hidden = !pending;
      }
      if (tag) tag.hidden = !isDefault(r);
      if (mod) mod.hidden = !changed;
      r.classList.toggle('is-mod', changed);
      r.classList.toggle('is-def', isDefault(r));
    });
    var n = changedRows().length;
    if (bar) {
      bar.hidden = n === 0;
      bar.querySelector('[data-stp-count]').textContent = n;
      bar.querySelector('[data-stp-counttxt]').textContent = n === 1 ? 'unsaved change' : 'unsaved changes';
    }
  }
  // The Replies group is read-only until Edit is confirmed; leaving the tab or
  // reloading locks it again. Inert keeps the controls in the form, so a staged
  // reset still reaches the draft.
  function setLocked(on){
    locked = on;
    if (!repl) return;
    repl.classList.toggle('is-locked', on);
    repl.querySelectorAll('.stp-seg, [data-stp-switch]').forEach(function(el){
      if (on) el.setAttribute('inert', ''); else el.removeAttribute('inert');
    });
    var gr = repl.querySelector('[data-stp-groupreset]'), lb = repl.querySelector('[data-stp-replylock]');
    if (gr) gr.disabled = on;
    var chip = repl.querySelector('[data-stp-lockchip]');
    if (chip) { chip.textContent = on ? 'Locked' : 'Editing'; chip.classList.toggle('stp-chip--warn', !on); }
    if (lb) { lb.textContent = on ? 'Edit' : 'Lock'; lb.setAttribute('aria-pressed', on ? 'false' : 'true'); lb.classList.toggle('is-on', !on); }
    rows.forEach(function(r){ if (r.dataset.kind === 'sys') paint(r); });
  }

  // Puts one field back on its system default whatever the page opened with or
  // saved, and tells the server to leave it unset. The key reference empties;
  // the key itself lives in .env and is never touched.
  function resetRow(row){
    var k = row.dataset.kind;
    if (k === 'key') { row.querySelector('input[name="' + row.dataset.fld + '"]').value = ''; return; }
    if (k === 'sys') { setValue(row, 'default'); return; }
    useDefault(row);
  }
  function resetWithin(el){
    rows.forEach(function(r){ if (el.contains(r)) resetRow(r); });
    followProvider();
    refresh();
  }
  // Fields that have not been saved show the default of the provider in force,
  // so they move with it when the provider changes.
  var lastProv = provider();
  function followProvider(){
    var now = provider();
    if (now === lastProv) return;
    rows.forEach(function(r){
      if (!r.dataset.defaults || r.dataset.raw !== '') return;
      try {
        var m = JSON.parse(r.dataset.defaults), v = valueOf(r).replace(/\*$/, '');
        if (m[lastProv] !== undefined && v === m[lastProv] && m[now] !== undefined) {
          var star = isReset(r);
          setValue(r, m[now] + (star ? '*' : ''));
        }
      } catch (e) {}
    });
    lastProv = now;
  }
  var PLACE = ['weather.feed', 'weather.maze', 'weather.mode', 'weather.side'];
  var PREFS = {appearance: ['theme', 'font.'], widgets: ['clock.', 'weather.'], clock: ['clock.'], weather: ['weather.'], placement: PLACE};
  // Weather owns every weather.* key except the ones Placement holds.
  var EXCEPT = {weather: PLACE};
  function clearPrefs(kind){
    try {
      var drop = [];
      for (var i = 0; i < localStorage.length; i++) {
        var key = localStorage.key(i);
        if (PREFS[kind].some(function(p){ return key === p || (p.slice(-1) === '.' && key.indexOf(p) === 0); }) && (EXCEPT[kind] || []).indexOf(key) < 0) drop.push(key);
      }
      drop.forEach(function(k){ localStorage.removeItem(k); });
    } catch (e) {}
  }

  function clearErrors(){
    rows.forEach(function(r){
      r.classList.remove('is-bad');
      var e = ctl(r, '[data-stp-err]');
      if (e) { e.hidden = true; e.textContent = ''; }
    });
    if (banner) banner.hidden = true;
  }
  function showBanner(text, reload){
    if (!banner) return;
    banner.querySelector('[data-stp-bannertext]').textContent = text;
    banner.querySelector('[data-stp-reload]').hidden = !reload;
    banner.hidden = false;
    window.scrollTo({top: 0, behavior: 'smooth'});
  }
  // Errors from the server land under the row they name. The first one's tab
  // opens so it is seen; anything without a row goes in the banner.
  function showErrors(list, message){
    clearErrors();
    var first = null, stray = [];
    (list || []).forEach(function(e){
      var row = rows.filter(function(r){ return r.dataset.fld === e.field; })[0];
      if (!row) { stray.push(e.msg); return; }
      row.classList.add('is-bad');
      var el = ctl(row, '[data-stp-err]');
      el.textContent = e.msg;
      el.hidden = false;
      first = first || row;
    });
    if (first) {
      var pane = first.closest('[data-stp-pane]').dataset.stpPane;
      if (pane !== current && show(pane)) history.pushState({stp: pane}, '', '/settings/' + pane);
      first.scrollIntoView({block: 'center', behavior: 'smooth'});
    }
    if (stray.length || (!first && message)) showBanner(stray.join(' ') || message, false);
  }

  function post(url, body){
    return fetch(url, {method: 'POST', body: body, credentials: 'same-origin', headers: {Accept: 'application/json, text/html'}});
  }
  function draft(){ return new URLSearchParams(new FormData(form)); }
  function reloadWith(msg){
    try { sessionStorage.setItem('stp.applied', msg); } catch (e) {}
    skipGuard = true;
    window.location.reload();
  }
  function reply(res){
    return res.text().then(function(t){
      var j = null;
      try { j = JSON.parse(t); } catch (e) {}
      return {res: res, json: j, text: t};
    });
  }
  // A failure every editing call can come back with.
  function failed(r){
    if (r.res.status === 409 && r.json && r.json.conflict) { closeModal(); showBanner(r.json.message, true); return true; }
    if (r.res.status === 422 && r.json && r.json.errors) { closeModal(); showErrors(r.json.errors, r.json.message); return true; }
    if (!r.res.ok) { closeModal(); showBanner((r.json && r.json.message) || 'That did not work. Try again.', false); return true; }
    return false;
  }
  function closeModal(){ if (modal) modal.innerHTML = ''; }

  if (form) {
    rows.forEach(function(r){ snap[r.dataset.fld] = valueOf(r); });
    setLocked(true);
    rows.forEach(paint);
    refresh();
    var pend = null;
    try { pend = JSON.parse(sessionStorage.getItem('stp.restage')); sessionStorage.removeItem('stp.restage'); } catch (e) {}
    (pend || []).sort(function(a, b){ return (b === 'scoring') - (a === 'scoring'); }).forEach(function(slug){
      var pane = root.querySelector('[data-stp-pane="' + slug + '"]');
      if (pane) resetWithin(pane);
    });
    form.addEventListener('submit', function(e){ e.preventDefault(); });
    form.addEventListener('input', function(e){
      var row = e.target.closest('[data-fld]');
      if (row && e.target.tagName === 'TEXTAREA') e.target.dataset.draft = e.target.value;
      if (row && e.target.name !== row.dataset.fld + '__reset' && resetInput(row)) resetInput(row).value = '0';
      if (row) { paint(row); row.classList.remove('is-bad'); var er = ctl(row, '[data-stp-err]'); if (er) er.hidden = true; }
      refresh();
    });
    form.addEventListener('change', function(e){
      var row = e.target.closest('[data-fld]');
      if (row && resetInput(row)) resetInput(row).value = '0';
      if (row && row.dataset.kind === 'sys' && e.target.type === 'radio' && e.target.value === 'custom') {
        var ta = row.querySelector('textarea');
        if (!ta.dataset.draft.trim()) ta.dataset.draft = ta.dataset.builtin;
      }
      if (row) paint(row);
      followProvider();
      refresh();
    });
    form.addEventListener('click', function(e){
      var row = e.target.closest('[data-fld]');
      if (row && e.target.closest('[data-stp-usedefault]')) { useDefault(row); refresh(); return; }
      var rad = e.target.closest('input[type=radio]');
      if (row && rad && rad.name === 'inference.system_prompt' && rad.value === 'none' && row.dataset.mode !== 'none') {
        e.preventDefault();
        ask('Send no instructions? Use this only when the model carries its own (for example an Ollama Modelfile SYSTEM prompt). With structured replies off, nothing tells the model the reply format.',
          function(){ rad.checked = true; rad.dispatchEvent(new Event('change', {bubbles: true})); },
          {title: 'Send no instructions?', yes: 'Send none', no: 'Cancel', stay: true});
        return;
      }
      var sw = e.target.closest('[data-stp-switch]');
      if (row && sw) {
        var h = row.querySelector('input[name="' + row.dataset.fld + '"]');
        var flip = function(){
          h.value = h.value === 'true' ? 'false' : 'true';
          resetInput(row).value = '0';
          paint(row); refresh();
        };
        if (row.dataset.fld === 'inference.structured_output' && h.value === 'true') {
          ask('The app then digs the scores out of free text, which fails more often.', flip,
            {title: 'Turn off structured replies?', yes: 'Turn off', no: 'Cancel', stay: true});
          return;
        }
        flip();
        return;
      }
      var gr = e.target.closest('[data-stp-groupreset]');
      if (gr) {
        var grp = gr.closest('.stp-group'), h3 = grp.querySelector('h3');
        var nm = (h3.childNodes[0] ? h3.childNodes[0].textContent : h3.textContent).trim();
        ask('Reset ' + nm + ' to defaults? You can review before saving.', function(){ resetWithin(grp); },
          {title: 'Reset to default?', yes: 'Reset', no: 'Cancel', stay: true});
        return;
      }
      if (e.target.closest('[data-stp-replylock]')) {
        if (!locked) { setLocked(true); return; }
        ask('These settings decide how the model is told to answer. A wrong change can make every score fail. You can roll back from Saved versions.',
          function(){ setLocked(false); },
          {title: 'Edit reply settings?', yes: 'Edit anyway', no: 'Cancel', stay: true});
        return;
      }
      var less = e.target.closest('[data-stp-less]'), more = e.target.closest('[data-stp-more]');
      if (row && (less || more)) {
        var st = ctl(row, '.stp-step'), hi = row.querySelector('input[name="' + row.dataset.fld + '"]');
        resetInput(row).value = '0';
        var n = parseInt(hi.value, 10) + (more ? 1 : -1);
        n = Math.max(parseInt(st.dataset.min, 10), Math.min(parseInt(st.dataset.max, 10), n));
        hi.value = String(n);
        paint(row); refresh();
        return;
      }
      if (e.target.closest('[data-stp-discard]')) {
        rows.forEach(function(r){ setValue(r, snap[r.dataset.fld]); });
        clearErrors(); refresh();
        return;
      }
      if (e.target.closest('[data-stp-review]')) { review(); return; }
      if (e.target.closest('[data-stp-test]')) { testConnection(); return; }
      if (e.target.closest('[data-stp-paste]')) {
        var pb = form.querySelector('[data-stp-pastebox]');
        pb.hidden = !pb.hidden;
        if (!pb.hidden) pb.querySelector('[data-stp-pasteval]').focus();
        return;
      }
      if (e.target.closest('[data-stp-pastesave]')) { pasteKey(); return; }
      var cmp = e.target.closest('[data-stp-compare]');
      if (cmp) { compare(cmp); return; }
      var rb = e.target.closest('[data-stp-rollback]');
      if (rb) { rollback(rb.dataset.stpRollback); return; }
      if (e.target.closest('[data-stp-importcfg]')) { openImport(); return; }
      if (e.target.closest('[data-stp-reload]')) { skipGuard = true; window.location.reload(); }
    });
    var shown = null;
    try { shown = sessionStorage.getItem('stp.applied'); sessionStorage.removeItem('stp.applied'); } catch (e) {}
    if (shown && applied) { applied.textContent = shown; applied.hidden = false; }
  }
  var reloadBtn = banner && banner.querySelector('[data-stp-reload]');
  if (reloadBtn) reloadBtn.addEventListener('click', function(){ skipGuard = true; window.location.reload(); });

  function review(){
    clearErrors();
    post('/settings/review', draft()).then(reply).then(function(r){
      if (failed(r)) return;
      if (modal) modal.innerHTML = r.text;
    }).catch(function(){ showBanner('Could not reach the server.', false); });
  }

  // Review dialog and import dialog live in #modal; their buttons are handled
  // here by delegation.
  document.addEventListener('click', function(e){
    var t = e.target;
    if (!modal || !modal.contains(t)) return;
    var conf = t.closest('[data-stp-confirmhost]');
    if (conf) {
      var on = conf.getAttribute('aria-checked') !== 'true';
      conf.setAttribute('aria-checked', on ? 'true' : 'false');
      var go = modal.querySelector('[data-stp-savenow]') || modal.querySelector('[data-stp-importapply]');
      if (go) go.disabled = !on;
      return;
    }
    if (t.closest('[data-stp-resetgo]')) { resetDialog(t); return; }
    if (t.closest('[data-stp-savenow]')) { saveNow(); return; }
    if (t.closest('[data-stp-importpick]')) { modal.querySelector('[data-stp-importpickin]').click(); return; }
    if (t.closest('[data-stp-importpreview]')) { importPreview(); return; }
    if (t.closest('[data-stp-importapply]')) { importApply(); return; }
  });
  document.addEventListener('change', function(e){
    if (modal && modal.contains(e.target) && e.target.matches('[data-stp-pill], [data-stp-pillall]')) { resetDialog(e.target); return; }
    if (!modal || !e.target.matches('[data-stp-importpickin]') || !modal.contains(e.target)) return;
    var f = e.target.files[0];
    if (!f) return;
    f.text().then(function(txt){ modal.querySelector('[data-stp-importtext]').value = txt; importPreview(); });
  });

  function saveNow(){
    var body = draft(), conf = modal && modal.querySelector('[data-stp-confirmhost]');
    if (conf && conf.getAttribute('aria-checked') === 'true') body.set('confirm_host', '1');
    var btn = modal.querySelector('[data-stp-savenow]');
    if (btn) btn.disabled = true;
    post('/settings/save', body).then(reply).then(function(r){
      if (failed(r)) return;
      reloadWith(r.json.message);
    }).catch(function(){ closeModal(); showBanner('Could not reach the server.', false); });
  }

  function testConnection(){
    var out = form.querySelector('[data-stp-testout]'), msg = form.querySelector('[data-stp-testmsg]'), btn = form.querySelector('[data-stp-test]');
    function show(cls, short, long){
      out.hidden = false; out.className = 'stp-test ' + cls; out.textContent = short;
      msg.hidden = !long; msg.textContent = long || ''; msg.className = 'stp-row__h stp-test__msg' + (cls === 'is-bad' ? ' is-bad' : '');
    }
    show('is-wait', 'Testing...');
    btn.disabled = true;
    post('/settings/test', draft()).then(reply).then(function(r){
      btn.disabled = false;
      if (r.res.status === 422 && r.json && r.json.errors) {
        out.hidden = true; msg.hidden = true; showErrors(r.json.errors, r.json.message); return;
      }
      var j = r.json || {};
      show(j.ok ? 'is-ok' : 'is-bad', j.ok ? (j.ms ? 'Reachable, ' + j.ms + ' ms' : 'OK') : 'Failed', j.message || 'No answer.');
    }).catch(function(){ btn.disabled = false; show('is-bad', 'Failed', 'Could not reach the server.'); });
  }

  // Pasting a key writes it to .env on the server and hands back only the name
  // of the variable, which goes into the draft like any other edit.
  function pasteKey(){
    var box = form.querySelector('[data-stp-pastebox]'), val = box.querySelector('[data-stp-pasteval]');
    var err = box.querySelector('[data-stp-pasteerr]');
    err.hidden = true;
    var body = new URLSearchParams();
    body.set('value', val.value);
    body.set('expected_version', form.dataset.version);
    post('/settings/key', body).then(reply).then(function(r){
      val.value = '';
      var j = r.json || {};
      if (r.res.status === 409 && j.conflict) { showBanner(j.message, true); return; }
      if (!r.res.ok || !j.ok) { err.textContent = j.message || 'Could not save the key.'; err.hidden = false; return; }
      var row = form.querySelector('[data-fld="inference.api_key"]');
      row.querySelector('input[name="inference.api_key"]').value = j.ref;
      var chip = row.querySelector('[data-stp-keychip]');
      chip.className = 'stp-chip stp-chip--' + (j.tone || '');
      chip.textContent = j.chip;
      chip.dataset.was = chip.className + '|' + chip.textContent;
      row.dataset.pasted = '1';
      box.hidden = true;
      refresh();
    }).catch(function(){ err.textContent = 'Could not reach the server.'; err.hidden = false; });
  }

  function compare(btn){
    var li = btn.closest('[data-stp-ver]'), host = li.querySelector('[data-stp-diffhost]');
    var open = btn.getAttribute('aria-expanded') === 'true';
    if (open) { host.hidden = true; btn.setAttribute('aria-expanded', 'false'); return; }
    fetch('/settings/versions/' + btn.dataset.stpCompare + '/diff', {credentials: 'same-origin'})
      .then(function(r){ return r.ok ? r.text() : Promise.reject(); })
      .then(function(html){ host.innerHTML = html; host.hidden = false; btn.setAttribute('aria-expanded', 'true'); })
      .catch(function(){ host.textContent = 'Could not load that version.'; host.hidden = false; });
  }
  function rollback(version){
    var go = function(confirmed){
      var body = new URLSearchParams();
      body.set('version', version);
      body.set('expected_version', form.dataset.version);
      if (confirmed) body.set('confirm_host', '1');
      post('/settings/rollback', body).then(reply).then(function(r){
        var j = r.json || {};
        if (r.res.status === 422 && j.errors && j.errors[0].field === 'inference.host') {
          ask(j.message, function(){ go(true); },
            {title: 'Send the API key to a new address?', yes: 'Send the key and roll back', no: 'Cancel'});
          return;
        }
        if (failed(r)) return;
        if (j.noop) { showBanner(j.message, false); return; }
        reloadWith(j.message);
      }).catch(function(){ showBanner('Could not reach the server.', false); });
    };
    if (dirty()) ask('Rolling back replaces the changes you have not saved.', function(){ go(false); }); else go(false);
  }

  // ---- reset to defaults ---------------------------------------------------
  // General, Scoring and Advanced are staged in the form like any edit. Profile
  // and the browser-only looks apply at once and reload the page; a staged
  // reset rides through the reload in sessionStorage.
  var LABEL = {general: 'General', scoring: 'Scoring', advanced: 'Advanced', profile: 'Profile', appearance: 'Appearance', widgets: 'Widgets', clock: 'Clock', weather: 'Weather', placement: 'Placement'};
  var NOW = ['profile', 'appearance', 'widgets'];
  function names(list){ return list.map(function(k){ return LABEL[k]; }).join(', '); }
  function pillsOn(dlg){
    return Array.prototype.slice.call(dlg.querySelectorAll('[data-stp-pill]:checked')).map(function(b){ return b.dataset.stpPill; });
  }
  function resetDialog(t){
    var dlg = modal.querySelector('[data-stp-resetdlg]');
    if (!dlg) return;
    var pills = Array.prototype.slice.call(dlg.querySelectorAll('[data-stp-pill]')), all = dlg.querySelector('[data-stp-pillall]');
    if (t.closest('[data-stp-pillall]')) pills.forEach(function(b){ b.checked = all.checked; });
    if (t.closest('[data-stp-resetgo]')) { doReset(pillsOn(dlg)); return; }
    var sel = pillsOn(dlg);
    all.checked = sel.length === pills.length;
    all.indeterminate = sel.length > 0 && sel.length < pills.length;
    var now = sel.filter(function(k){ return NOW.indexOf(k) >= 0; }), wait = sel.filter(function(k){ return NOW.indexOf(k) < 0; });
    var parts = [];
    if (now.length) parts.push('Applies now: ' + names(now) + '.');
    if (wait.length) parts.push('Waits for Review and save: ' + names(wait) + '.');
    dlg.querySelector('[data-stp-resetsum]').textContent = parts.length ? parts.join(' ') : 'Nothing selected.';
    dlg.querySelector('[data-stp-resetwarn]').hidden = sel.indexOf('advanced') < 0;
    dlg.parentNode.querySelector('[data-stp-resetgo]').disabled = !sel.length;
  }
  function doReset(sel){
    var dlg = modal.querySelector('[data-stp-resetdlg]'), pid = dlg.dataset.defaultProfile;
    var stage = sel.filter(function(k){ return NOW.indexOf(k) < 0; }), now = sel.filter(function(k){ return NOW.indexOf(k) >= 0; });
    closeModal();
    stage.sort(function(a, b){ return (b === 'scoring') - (a === 'scoring'); });
    if (!now.length) {
      stage.forEach(function(k){ resetWithin(root.querySelector('[data-stp-pane="' + k + '"]')); });
      return;
    }
    var run = function(){
      var p = now.indexOf('profile') >= 0 ? post('/profiles/' + encodeURIComponent(pid) + '/active', new URLSearchParams()) : Promise.resolve({ok: true});
      p.then(function(res){
        if (!res.ok) { showBanner('Could not switch the profile.', false); return; }
        now.forEach(function(k){ if (PREFS[k]) clearPrefs(k); });
        try { if (stage.length) sessionStorage.setItem('stp.restage', JSON.stringify(stage)); } catch (e) {}
        reloadWith('Reset: ' + names(sel) + '.');
      }).catch(function(){ showBanner('Could not reach the server.', false); });
    };
    if (dirty()) ask('This applies now and reloads the page. Changes you have not saved are dropped.', run,
      {title: 'Reset and reload?', yes: 'Reset', no: 'Cancel'});
    else run();
  }
  root.addEventListener('click', function(e){
    var open = e.target.closest('[data-stp-resetall]');
    if (open) {
      var tpl = root.querySelector('[data-stp-resettpl]');
      if (tpl && modal) modal.innerHTML = tpl.innerHTML;
      return;
    }
    var loc = e.target.closest('[data-stp-resetlocal]');
    if (!loc) return;
    var kind = loc.dataset.stpResetlocal;
    ask('Reset ' + LABEL[kind] + ' to default? This applies now, this browser only.' + (dirty() ? ' Changes you have not saved are dropped.' : ''),
      function(){ clearPrefs(kind); reloadWith(LABEL[kind] + ' reset to default.'); },
      {title: 'Reset to default?', yes: 'Reset', no: 'Cancel'});
  });

  // ---- import ----------------------------------------------------------
  function openImport(){
    fetch('/settings/import', {credentials: 'same-origin'}).then(function(r){ return r.ok ? r.text() : Promise.reject(); })
      .then(function(html){ if (modal) modal.innerHTML = html; })
      .catch(function(){ showBanner('Could not open the import dialog.', false); });
  }
  function importBody(){
    var b = new URLSearchParams();
    b.set('yaml', modal.querySelector('[data-stp-importtext]').value);
    b.set('expected_version', form.dataset.version);
    var conf = modal.querySelector('[data-stp-confirmhost]');
    if (conf && conf.getAttribute('aria-checked') === 'true') b.set('confirm_host', '1');
    return b;
  }
  function importPreview(){
    var out = modal.querySelector('[data-stp-importresult]'), apply = modal.querySelector('[data-stp-importapply]');
    apply.disabled = true;
    post('/settings/import/preview', importBody()).then(reply).then(function(r){
      out.innerHTML = r.text;
      var ok = out.querySelector('[data-stp-importok]');
      if (ok && !ok.hasAttribute('data-nochange')) {
        apply.disabled = ok.hasAttribute('data-hostmoved');
      }
    }).catch(function(){ out.textContent = 'Could not reach the server.'; });
  }
  function importApply(){
    post('/settings/import/apply', importBody()).then(reply).then(function(r){
      var j = r.json || {};
      if (r.res.status === 409) { closeModal(); showBanner(j.message, true); return; }
      if (!r.res.ok) {
        var out = modal.querySelector('[data-stp-importresult]');
        out.innerHTML = '';
        var p = document.createElement('p'); p.className = 'stp-row__err'; p.setAttribute('role', 'alert'); p.textContent = j.message || 'Could not import that file.';
        out.appendChild(p); return;
      }
      reloadWith(j.message);
    });
  }

  // ---- leaving with unsaved edits ----------------------------------------
  // Holds an action until the person picks. Tab switches never come here.
  function ask(text, proceed, labels){
    if (!askLayer) { proceed(); return; }
    labels = labels || {title: 'You have unsaved changes', yes: 'Discard', no: 'Keep editing'};
    askLayer.innerHTML =
      '<div class="modal">' +
        '<div class="modal__backdrop" data-modal-close></div>' +
        '<div class="modal__frame modal__frame--ask" role="alertdialog" aria-modal="true" aria-labelledby="stpLeaveTitle">' +
          '<div class="modal__bar"><span class="modal__title" id="stpLeaveTitle"></span>' +
          '<button class="modal__close" type="button" data-modal-close aria-label="Close">&times;</button></div>' +
          '<div class="stp-dlg"><p class="stp-dlg__lead"></p></div>' +
          '<div class="modal__foot stp-dlg__foot">' +
            '<button class="clk-btn" type="button" data-modal-close data-stp-no></button>' +
            '<button class="clk-btn" type="button" data-stp-leave></button>' +
          '</div>' +
        '</div>' +
      '</div>';
    askLayer.querySelector('.stp-dlg__lead').textContent = text;
    askLayer.querySelector('#stpLeaveTitle').textContent = labels.title;
    askLayer.querySelector('[data-stp-no]').textContent = labels.no;
    askLayer.querySelector('[data-stp-leave]').textContent = labels.yes;
    askLayer.querySelector('[data-stp-leave]').classList.add(/^(Discard|Reset)/.test(labels.yes) ? 'clk-btn--danger' : 'clk-btn--go');
    askLayer.querySelector('[data-stp-leave]').addEventListener('click', function(){
      askLayer.innerHTML = '';
      if (!labels.stay) skipGuard = true;
      proceed();
    });
  }
  document.addEventListener('click', function(e){
    var link = e.target.closest('a[href]');
    if (!link || link.closest('[data-stp-tab]') || link.hasAttribute('data-stp-tab')) return;
    if (e.defaultPrevented || e.button !== 0 || e.ctrlKey || e.metaKey || e.shiftKey || e.altKey) return;
    if (link.target && link.target !== '_self') return;
    if (link.hasAttribute('download') || link.origin !== window.location.origin || link.hasAttribute('hx-get') || !dirty()) return;
    if (link.getAttribute('href').charAt(0) === '#') return;
    e.preventDefault();
    ask('Your settings changes are not saved yet.', function(){ window.location.href = link.href; });
  });
  window.addEventListener('beforeunload', function(e){
    if (!dirty()) return;
    e.preventDefault();
    e.returnValue = '';
  });

  // ---- feeds on General --------------------------------------------------
  var count = root.querySelector('[data-stp-feedcount]');
  var msg = root.querySelector('[data-stp-importmsg]');
  var file = root.querySelector('[data-stp-importfile]');
  var importBtn = root.querySelector('[data-stp-import]');

  function showCount(n){
    if (count && n !== null && n !== '') count.textContent = n;
  }
  function say(text, bad){
    if (!msg) return;
    msg.textContent = text;
    msg.classList.toggle('is-bad', !!bad);
    msg.hidden = !text;
  }
  if (importBtn && file){
    importBtn.addEventListener('click', function(){ file.click(); });
    file.addEventListener('change', function(){
      var chosen = file.files[0];
      file.value = '';
      if (!chosen) return;
      var body = new FormData();
      body.append('file', chosen);
      fetch('/sources/import', {method: 'POST', body: body, credentials: 'same-origin'})
        .then(function(res){
          if (res.redirected || new URL(res.url).pathname !== '/sources/import'){
            window.location.reload();
            return null;
          }
          if (!res.ok) throw new Error('status ' + res.status);
          showCount(res.headers.get('X-Feed-Count'));
          return res.text();
        })
        .then(function(html){
          if (html === null) return;
          var doc = new DOMParser().parseFromString(html, 'text/html');
          var note = doc.querySelector('.src__import');
          say(note ? note.textContent.trim() : 'Feeds imported.', !!doc.querySelector('.sdet__err'));
        })
        .catch(function(){ say('Could not import the feeds file.', true); });
    });
  }
  // Every Sources dialog response carries the feed count.
  document.addEventListener('htmx:afterRequest', function(e){
    var xhr = e.detail && e.detail.xhr;
    if (xhr) showCount(xhr.getResponseHeader('X-Feed-Count'));
  });

  // ---- "?" help bubbles ---------------------------------------------------
  // CSS opens a bubble on hover or focus; this places it beside its button
  // inside the viewport, toggles it on a tap and closes it on Escape, a click
  // elsewhere or a scroll.
  function tipOf(btn){ return btn.nextElementSibling; }
  function place(btn){
    var tip = tipOf(btn), r = btn.getBoundingClientRect(), vw = document.documentElement.clientWidth;
    var w = tip.offsetWidth, h = tip.offsetHeight, m = 8;
    var left = Math.max(m, Math.min(r.left, vw - w - m));
    var top = r.bottom + 6;
    if (top + h > window.innerHeight - m && r.top - h - 6 > m) top = r.top - h - 6;
    tip.style.left = left + 'px';
    tip.style.top = top + 'px';
  }
  function closeHints(except){
    root.querySelectorAll('[data-hint].is-open').forEach(function(b){
      if (b !== except) b.classList.remove('is-open');
    });
  }
  root.addEventListener('mouseover', function(e){
    var b = e.target.closest && e.target.closest('[data-hint]');
    if (b) place(b);
  });
  root.addEventListener('focusin', function(e){
    var b = e.target.closest && e.target.closest('[data-hint]');
    if (b) place(b);
  });
  root.addEventListener('click', function(e){
    var b = e.target.closest && e.target.closest('[data-hint]');
    closeHints(b);
    if (!b) return;
    place(b);
    b.classList.toggle('is-open');
  });
  document.addEventListener('keydown', function(e){
    if (e.key === 'Escape') {
      closeHints();
      var a = document.activeElement;
      if (a && a.matches && a.matches('[data-hint]')) a.blur();
    }
  });
  document.addEventListener('click', function(e){
    if (!root.contains(e.target)) closeHints();
  });
  window.addEventListener('scroll', function(){ closeHints(); }, {passive: true});
})();

// A themed listbox for long option lists (the date format): a button showing
// the pick, and a popup of options that each carry their own live preview. The
// choice itself lives in hidden radios inside the menu, so the code that owns
// the preference keeps reading and writing plain radios; picking an option
// checks its radio and fires change.
(function(){
  var menus = Array.prototype.slice.call(document.querySelectorAll('[data-stp-menu]'));
  if (!menus.length) return;
  var openMenu = null;

  function opts(m){ return Array.prototype.slice.call(m.querySelectorAll('[role="option"]')); }
  function radio(m, v){
    return Array.prototype.slice.call(m.querySelectorAll('input[type="radio"]')).filter(function(r){ return r.value === v; })[0];
  }
  function picked(m){
    var r = m.querySelector('input[type="radio"]:checked');
    return r ? r.value : null;
  }
  function sync(m){
    var v = picked(m), cur = m.querySelector('[data-stp-menucur]'), on = null;
    opts(m).forEach(function(o){
      var is = o.dataset.value === v;
      o.setAttribute('aria-selected', is ? 'true' : 'false');
      if (is) on = o;
    });
    if (cur) {
      var p = on && (on.querySelector('.stp-menu__p') || on.querySelector('.stp-menu__n'));
      cur.textContent = p && p.textContent ? p.textContent : (on ? on.dataset.value : '');
    }
  }
  function place(m){
    var b = m.querySelector('[data-stp-menubtn]'), l = m.querySelector('[role="listbox"]');
    var r = b.getBoundingClientRect(), vh = window.innerHeight, h = l.offsetHeight;
    l.style.width = r.width + 'px';
    l.style.left = r.left + 'px';
    l.style.top = (r.bottom + 6 + h > vh - 8 && r.top - h - 6 > 8 ? r.top - h - 6 : r.bottom + 6) + 'px';
  }
  function active(m, o){
    var l = m.querySelector('[role="listbox"]');
    opts(m).forEach(function(x){ x.classList.toggle('is-active', x === o); });
    if (o) { l.setAttribute('aria-activedescendant', o.id); o.scrollIntoView({block: 'nearest'}); }
  }
  function close(m, back){
    if (!m) return;
    var b = m.querySelector('[data-stp-menubtn]'), l = m.querySelector('[role="listbox"]');
    l.hidden = true;
    b.setAttribute('aria-expanded', 'false');
    if (openMenu === m) openMenu = null;
    if (back) b.focus();
  }
  function open(m){
    if (openMenu && openMenu !== m) close(openMenu);
    var b = m.querySelector('[data-stp-menubtn]'), l = m.querySelector('[role="listbox"]');
    sync(m);
    l.hidden = false;
    b.setAttribute('aria-expanded', 'true');
    place(m);
    openMenu = m;
    l.focus({preventScroll: true});
    active(m, opts(m).filter(function(o){ return o.getAttribute('aria-selected') === 'true'; })[0] || opts(m)[0]);
  }
  function choose(m, o){
    var r = radio(m, o.dataset.value);
    if (r && !r.checked) {
      r.checked = true;
      r.dispatchEvent(new Event('change', {bubbles: true}));
    }
    sync(m);
    close(m, true);
  }

  menus.forEach(function(m, mi){
    var b = m.querySelector('[data-stp-menubtn]'), l = m.querySelector('[role="listbox"]');
    opts(m).forEach(function(o, i){ o.id = 'stpMenu' + mi + 'o' + i; });
    b.addEventListener('click', function(){ if (l.hidden) open(m); else close(m); });
    b.addEventListener('keydown', function(e){
      if (e.key === 'ArrowDown' || e.key === 'ArrowUp') { e.preventDefault(); open(m); }
    });
    l.addEventListener('click', function(e){
      var o = e.target.closest('[role="option"]');
      if (o) choose(m, o);
    });
    l.addEventListener('mousemove', function(e){
      var o = e.target.closest('[role="option"]');
      if (o) active(m, o);
    });
    l.addEventListener('keydown', function(e){
      var all = opts(m), i = all.indexOf(all.filter(function(o){ return o.classList.contains('is-active'); })[0]);
      if (e.key === 'ArrowDown') { e.preventDefault(); active(m, all[Math.min(all.length - 1, i + 1)]); }
      else if (e.key === 'ArrowUp') { e.preventDefault(); active(m, all[Math.max(0, i - 1)]); }
      else if (e.key === 'Home') { e.preventDefault(); active(m, all[0]); }
      else if (e.key === 'End') { e.preventDefault(); active(m, all[all.length - 1]); }
      else if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); if (all[i]) choose(m, all[i]); }
      else if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); close(m, true); }
      else if (e.key === 'Tab') close(m);
    });
    m.addEventListener('change', function(){ sync(m); });
    sync(m);
  });
  document.addEventListener('click', function(e){
    if (openMenu && !openMenu.contains(e.target)) close(openMenu);
  });
  window.addEventListener('resize', function(){ close(openMenu); });
  window.addEventListener('scroll', function(e){
    if (openMenu && e.target.nodeType === 1 && openMenu.contains(e.target)) return;
    close(openMenu);
  }, {passive: true, capture: true});
  window.addEventListener('load', function(){ menus.forEach(sync); });
})();

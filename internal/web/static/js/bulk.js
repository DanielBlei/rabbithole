// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

(function(){
  var selectBar = document.querySelector('[data-bulk-selectbar]');
  var bulkBar = document.querySelector('[data-bulk-bar]');
  var countEl = document.querySelector('[data-bulk-count]');
  var statusEl = document.querySelector('[data-bulk-status]');
  var selectAll = document.querySelector('[data-bulk-select-all]');
  if (!selectBar || !bulkBar) return;

  var selected = new Set();
  var bulkInFlight = false;
  var bulkFailed = 0;

  function allRows(){
    return Array.prototype.slice.call(document.querySelectorAll('.pane__body .row'));
  }

  function visibleRows(){
    var rows = allRows();
    var vis = [];
    rows.forEach(function(row){
      if (row.classList.contains('row--filtered')) return;
      var unit = row.closest('.story-group') || row;
      if (unit.style.display === 'none') return;
      var cs = window.getComputedStyle(unit);
      if (cs.display === 'none') return;
      vis.push(row);
    });
    return vis;
  }

  function updateUI(){
    var vis = visibleRows();
    var totalVis = vis.length;
    var visMeta = document.querySelector('[data-bulk-visible]');
    if (visMeta) {
      visMeta.textContent = totalVis ? '(' + totalVis + ' visible)' : '';
    }
    var count = selected.size;
    if (countEl) countEl.textContent = count ? count + ' selected' : '';
    if (bulkBar) bulkBar.hidden = count === 0 && bulkFailed === 0;
    if (selectAll) {
      var selVis = 0;
      vis.forEach(function(r){ if (selected.has(r.dataset.id)) selVis++; });
      if (totalVis === 0 || selVis === 0) {
        selectAll.checked = false;
        selectAll.indeterminate = false;
      } else if (selVis === totalVis) {
        selectAll.checked = true;
        selectAll.indeterminate = false;
      } else {
        selectAll.checked = false;
        selectAll.indeterminate = true;
      }
    }
    allRows().forEach(function(row){
      var cb = row.querySelector('[data-bulk-check]');
      if (!cb) return;
      cb.checked = selected.has(row.dataset.id);
    });
  }

  function clearSelection(){
    if (bulkInFlight) return;
    if (selected.size === 0 && bulkFailed === 0) return;
    selected.clear();
    bulkFailed = 0;
    if (statusEl) statusEl.textContent = '';
    updateUI();
  }

  function toggleRow(id, checked){
    if (checked) selected.add(id);
    else selected.delete(id);
    bulkFailed = 0;
    if (statusEl) statusEl.textContent = '';
    updateUI();
  }

  document.addEventListener('change', function(e){
    var cb = e.target.closest && e.target.closest('[data-bulk-check]');
    if (!cb) return;
    var row = cb.closest('.row');
    if (!row) return;
    var id = row.dataset.id || cb.value;
    toggleRow(id, cb.checked);
    e.stopPropagation();
  });

  if (selectAll) {
    selectAll.addEventListener('change', function(){
      var vis = visibleRows();
      if (selectAll.checked) {
        vis.forEach(function(row){ selected.add(row.dataset.id); });
      } else {
        vis.forEach(function(row){ selected.delete(row.dataset.id); });
      }
      bulkFailed = 0;
      if (statusEl) statusEl.textContent = '';
      updateUI();
    });
  }

  document.addEventListener('keydown', function(e){
    if (e.key !== 'Escape') return;
    if (selected.size === 0) return;
    clearSelection();
    e.preventDefault();
  });

  var filterForm = document.querySelector('.pane__filters');
  if (filterForm) {
    filterForm.addEventListener('change', function(){ clearSelection(); });
    filterForm.addEventListener('submit', function(){ clearSelection(); });
  }
  document.querySelectorAll('#btnPrev, #btnNext, #btnFirst, #btnPrevBottom, #btnNextBottom, [data-rows-btn]').forEach(function(btn){
    btn.addEventListener('click', function(){ clearSelection(); });
  });
  var searchInput = document.querySelector('[data-search-query]');
  if (searchInput) {
    searchInput.addEventListener('input', function(){ clearSelection(); });
  }
  document.addEventListener('click', function(e){
    var clear = e.target.closest('[data-search-clear]');
    if (clear) clearSelection();
  });
  document.addEventListener('htmx:afterSwap', function(e){
    var t = e.detail && e.detail.target;
    if (!t || !t.classList || !t.classList.contains('row')) return;
    if (bulkInFlight) {
      updateUI();
      return;
    }
    if (selected.size) clearSelection();
    updateUI();
  });

  function rowState(row){
    return {
      hidden: row.classList.contains('is-hidden'),
      seen: row.classList.contains('is-seen'),
      bookmarked: (function(){ var b = row.querySelector('.bookmark'); return !!(b && b.classList.contains('is-saved')); })()
    };
  }

  function urlFor(id, action, row){
    if (action === 'bookmark') return '/items/' + encodeURIComponent(id) + '/bookmark';
    if (action === 'seen') return '/items/' + encodeURIComponent(id) + '/seen';
    if (action === 'hide') return '/items/' + encodeURIComponent(id) + '/hide';
    if (action === 'unhide') {
      var st = row ? rowState(row) : {hidden:false};
      if (st.hidden) return '/items/' + encodeURIComponent(id) + '/hide';
      return null;
    }
    return null;
  }

  function shouldPost(row, action){
    if (!row) return true;
    var st = rowState(row);
    if (action === 'bookmark') return !st.bookmarked;
    if (action === 'seen') return !st.seen;
    if (action === 'hide') return !st.hidden;
    if (action === 'unhide') return st.hidden;
    return true;
  }

  bulkBar.addEventListener('click', function(e){
    var btn = e.target.closest('[data-bulk-action]');
    if (!btn) return;
    if (bulkInFlight) return;
    var action = btn.getAttribute('data-bulk-action');
    var ids = Array.from(selected);
    if (!ids.length) return;
    var byId = {};
    allRows().forEach(function(row){ byId[row.dataset.id] = row; });
    var tasks = [];
    ids.forEach(function(id){
      var row = byId[id];
      if (!row) return;
      var url = urlFor(id, action, row);
      if (!url) return;
      if (!shouldPost(row, action)) return;
      tasks.push({id:id, url:url, row:row});
    });
    if (tasks.length === 0) {
      selected.clear();
      bulkFailed = 0;
      if (statusEl) statusEl.textContent = '';
      updateUI();
      return;
    }
    bulkInFlight = true;
    if (statusEl) statusEl.textContent = '';
    bulkBar.querySelectorAll('button').forEach(function(b){ b.disabled = true; });
    var failed = 0;
    Promise.all(tasks.map(function(t){
      return fetch(t.url, {method:'POST', credentials:'same-origin', headers:{'HX-Request':'true'}})
        .then(function(res){
          var redirect = res.headers.get('HX-Redirect');
          if (redirect) {
            window.location.href = redirect;
            throw new Error('redirect');
          }
          if (!res.ok) throw new Error('request failed: ' + res.status);
          return res.text();
        })
        .then(function(html){
          if (!html) return;
          var cur = document.querySelector('.pane__body .row[data-id="' + t.id + '"]');
          if (!cur) return;
          var temp = document.createElement('div');
          temp.innerHTML = html.trim();
          var next = temp.firstElementChild;
          if (!next) return;
          cur.outerHTML = next.outerHTML;
          var refreshed = document.querySelector('.pane__body .row[data-id="' + t.id + '"]');
          if (refreshed) {
            if (window.htmx) htmx.process(refreshed);
            var ev = new CustomEvent('htmx:afterSwap', {bubbles:true, detail:{target: refreshed}});
            document.dispatchEvent(ev);
          }
        })
        .catch(function(err){
          if (err && err.message === 'redirect') return;
          failed++;
        });
    })).then(function(){
      bulkInFlight = false;
      bulkFailed = failed;
      selected.clear();
      bulkBar.querySelectorAll('button').forEach(function(b){ b.disabled = false; });
      if (failed > 0 && statusEl) {
        statusEl.textContent = failed + ' failed — refresh and try again';
      } else if (statusEl) {
        statusEl.textContent = '';
      }
      updateUI();
    });
  });

  updateUI();
  var body = document.querySelector('.pane__body');
  if (body) {
    var observer = new MutationObserver(function(){ updateUI(); });
    observer.observe(body, {attributes:true, attributeFilter:['class','style'], subtree:true});
  }

})();

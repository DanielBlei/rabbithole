// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

(function(){
  var selectBar = document.querySelector('[data-bulk-selectbar]');
  var bulkBar = document.querySelector('[data-bulk-bar]');
  var countEl = document.querySelector('[data-bulk-count]');
  var selectAll = document.querySelector('[data-bulk-select-all]');
  if (!selectBar || !bulkBar) return;

  var selected = new Set();

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
    if (bulkBar) bulkBar.hidden = count === 0;
    if (selectAll) {
      var selVis = 0;
      vis.forEach(function(r){ if (selected.has(r.dataset.id)) selVis++; });
      if (totalVis === 0) {
        selectAll.checked = false;
        selectAll.indeterminate = false;
      } else if (selVis === 0) {
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
    // Sync row checkboxes
    allRows().forEach(function(row){
      var cb = row.querySelector('[data-bulk-check]');
      if (!cb) return;
      cb.checked = selected.has(row.dataset.id);
    });
  }

  function clearSelection(){
    if (selected.size === 0) return;
    selected.clear();
    updateUI();
  }

  function toggleRow(id, checked){
    if (checked) selected.add(id);
    else selected.delete(id);
    updateUI();
  }

  // Row checkbox changes (delegated)
  document.addEventListener('change', function(e){
    var cb = e.target.closest && e.target.closest('[data-bulk-check]');
    if (!cb) return;
    var row = cb.closest('.row');
    if (!row) return;
    var id = row.dataset.id || cb.value;
    toggleRow(id, cb.checked);
    e.stopPropagation();
  });

  // Select-all in header
  if (selectAll) {
    selectAll.addEventListener('change', function(){
      var vis = visibleRows();
      if (selectAll.checked) {
        vis.forEach(function(row){ selected.add(row.dataset.id); });
      } else {
        vis.forEach(function(row){ selected.delete(row.dataset.id); });
      }
      updateUI();
    });
  }

  // Escape clears selection
  document.addEventListener('keydown', function(e){
    if (e.key !== 'Escape') return;
    if (selected.size === 0) return;
    // Don't interfere if a modal or search suggestion is open and already handles Escape.
    // Bulk clear should run, but we allow higher-priority handlers to run first.
    // If selection exists, clear it and stop other Escape handlers from closing popmenus after.
    clearSelection();
    e.preventDefault();
  });

  // Clear on filter/sort/page/view changes - any change in the filter bar.
  var filterForm = document.querySelector('.pane__filters');
  if (filterForm) {
    filterForm.addEventListener('change', function(){ clearSelection(); });
    filterForm.addEventListener('submit', function(){ clearSelection(); });
  }
  // Pager buttons
  document.querySelectorAll('#btnPrev, #btnNext, #btnFirst, #btnPrevBottom, #btnNextBottom, [data-rows-btn]').forEach(function(btn){
    btn.addEventListener('click', function(){ clearSelection(); });
  });
  // Search input - typing filters locally
  var searchInput = document.querySelector('[data-search-query]');
  if (searchInput) {
    searchInput.addEventListener('input', function(){ clearSelection(); });
  }
  document.addEventListener('click', function(e){
    var clear = e.target.closest('[data-search-clear]');
    if (clear) clearSelection();
  });
  // htmx afterSwap for single-row actions should also clear stale selection
  document.addEventListener('htmx:afterSwap', function(e){
    var t = e.detail && e.detail.target;
    if (!t || !t.classList || !t.classList.contains('row')) return;
    // If a row swapped while bulk selection existed, clear to avoid stale ids.
    if (selected.size) clearSelection();
    // Re-sync UI because new row elements have fresh checkboxes
    updateUI();
  });

  // Bulk actions
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
      var st = row ? rowState(row) : {hidden:false, seen:false};
      if (st.hidden) return '/items/' + encodeURIComponent(id) + '/hide';
      if (st.seen) return '/items/' + encodeURIComponent(id) + '/seen';
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
    if (action === 'unhide') return st.hidden || st.seen;
    return true;
  }

  bulkBar.addEventListener('click', function(e){
    var btn = e.target.closest('[data-bulk-action]');
    if (!btn) return;
    var action = btn.getAttribute('data-bulk-action');
    var ids = Array.prototype.slice.call(selected);
    if (!ids.length) return;
    btn.disabled = true;
    var done = 0;
    var total = ids.length;
    var byId = {};
    allRows().forEach(function(row){ byId[row.dataset.id] = row; });

    // Filter to only rows that need action for idempotency; still count as attempted
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
      clearSelection();
      btn.disabled = false;
      return;
    }

    // Disable bulk bar while running
    bulkBar.querySelectorAll('button').forEach(function(b){ b.disabled = true; });

    Promise.all(tasks.map(function(t){
      return fetch(t.url, {method:'POST', credentials:'same-origin', headers:{'HX-Request':'true'}})
        .then(function(res){ if (!res.ok) throw new Error('request failed'); return res.text(); })
        .then(function(html){
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
            // Fire afterSwap so feed.js leave animation and pager refresh run
            var ev = new CustomEvent('htmx:afterSwap', {bubbles:true, detail:{target: refreshed}});
            document.dispatchEvent(ev);
            // Also trigger search re-apply via same event (feed.js listens)
          }
        })
        .catch(function(){})
        .then(function(){ done++; });
    })).then(function(){
      clearSelection();
      bulkBar.querySelectorAll('button').forEach(function(b){ b.disabled = false; });
      updateUI();
    });
  });

  // Initial sync
  updateUI();
  // Re-sync after pager/search render changes visibility
  var origRender = window.feedPager && window.feedPager.render;
  if (origRender) {
    // feedPager not yet available at defer time? bulk.js loads after feed.js, so it is.
  }
  // Observe DOM changes that affect visibility (pager hiding, filter class)
  var observer = new MutationObserver(function(){ updateUI(); });
  var body = document.querySelector('.pane__body');
  if (body) observer.observe(body, {attributes:true, attributeFilter:['class','style'], subtree:true});

})();

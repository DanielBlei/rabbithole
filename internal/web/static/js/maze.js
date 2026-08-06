// The Maze page: the tag filter, click-to-recolour tags, the idea board's
// drag-to-reorder, and the wheel gesture that swaps boards. The tag-chip input
// and the colour palette live in tagput.js, shared with the Sources page.
(function () {
  // The todo widget re-renders whole on every mutation, so init runs again on
  // each htmx swap; the tag filter's selection is kept in a closure Set so it
  // survives those swaps. All state lives in the DOM otherwise (no framework).

  // ---- tag chips ------------------------------------------------------
  // The chip input and the colour palette live in tagput.js, shared with the
  // Sources page. What stays here is the Maze's own use of them: recolouring a
  // task's tag by clicking it.
  function applyTagColors() {
    document.querySelectorAll('#todoWidget .todo__tag').forEach(function (el) {
      window.tagput.colorize(el, (el.dataset.tag || el.textContent).trim());
    });
    document.querySelectorAll('.pill--tag').forEach(function (el) {
      var s = el.querySelector('span');
      window.tagput.colorize(el, (s ? s.textContent : '').trim());
    });
    window.tagput.colorChips();
  }
  // Click a row tag chip to advance its colour (persists for that tag name).
  document.addEventListener('click', function (e) {
    var el = e.target.closest && e.target.closest('.todo__tag');
    if (!el) return;
    window.tagput.cycleColor((el.dataset.tag || el.textContent).trim());
    applyTagColors();
  });

  // Panels show via CSS :checked; radios can't uncheck themselves, so this only
  // handles the collapse-on-re-click case. State is stashed on the tab element
  // between its own mousedown and click (same element, so it survives). Same
  // handler the Feed's why/note tabs use.
  document.addEventListener('mousedown', function (e) {
    var tab = e.target.closest('.tg-tab');
    if (!tab) return;
    var input = document.getElementById(tab.getAttribute('for'));
    tab._open = !!(input && input.checked);
  });
  document.addEventListener('click', function (e) {
    var tab = e.target.closest('.tg-tab');
    if (!tab || !tab._open) return;
    var input = document.getElementById(tab.getAttribute('for'));
    if (input) { e.preventDefault(); input.checked = false; }
  });

  // ---- idea note: live colour preview ---------------------------------
  // Picking a swatch recolours its note immediately (the .note--<colour> class is
  // the single source of the card's look), so editing feels fluid; the server
  // records the choice on save. Delegated so it works on swapped-in notes too.
  document.addEventListener('change', function (e) {
    var inp = e.target;
    if (!inp.matches || !inp.matches('.idea__palette input[name=color]')) return;
    var note = inp.closest('.note');
    if (!note) return;
    Array.prototype.slice.call(note.classList).forEach(function (c) {
      if (c.indexOf('note--') === 0 && c !== 'note--new' && c !== 'note--dragging') note.classList.remove(c);
    });
    note.classList.add('note--' + inp.value);
  });

  // ---- swap boards by scrolling the empty frame ------------------------
  // Wheeling anywhere on the Maze page that ISN'T a board pane or the topbar —
  // the tab row, the page margins, the area below the panes — toggles between
  // the Tasks and Ideas boards; with two boards, either scroll direction flips
  // to the other one. A short cooldown keeps one gesture to one swap.
  // Scrolling over a board pane is left untouched.
  var lastSwap = 0;
  document.addEventListener('wheel', function (e) {
    // Don't flip boards while a modal is open.
    if (document.documentElement.classList.contains('modal-open')) return;
    if (!document.querySelector('.maze') || (e.target.closest && (e.target.closest('.pane') || e.target.closest('.topbar') || e.target.closest('#modal')))) return;
    var now = Date.now();
    if (now - lastSwap < 450) { e.preventDefault(); return; }
    var todos = document.getElementById('mazeTabTodos');
    var ideas = document.getElementById('mazeTabIdeas');
    if (!todos || !ideas || !e.deltaY) return;
    if (ideas.checked) { todos.checked = true; } else { ideas.checked = true; }
    lastSwap = now;
    e.preventDefault();
  }, { passive: false });

  // ---- tag filter (client-side OR, across all tab panels) -------------
  var selected = new Set();
  function applyFilter() {
    document.querySelectorAll('#todoWidget .todo').forEach(function (row) {
      if (!selected.size) { row.classList.remove('todo--filtered'); return; }
      var tags = (row.dataset.tags || '').split(',').filter(Boolean);
      var hit = tags.some(function (t) { return selected.has(t); });
      row.classList.toggle('todo--filtered', !hit);
    });
    var dot = document.querySelector('[data-tag-active]');
    if (dot) dot.hidden = selected.size === 0;
  }
  function initFilter() {
    var boxes = document.querySelectorAll('.todo-filter__chk');
    var exists = new Set();
    boxes.forEach(function (b) { exists.add(b.value.toLowerCase()); });
    Array.from(selected).forEach(function (t) { if (!exists.has(t)) selected.delete(t); });
    boxes.forEach(function (b) {
      var v = b.value.toLowerCase();
      b.checked = selected.has(v);
      b.addEventListener('change', function () {
        if (b.checked) selected.add(v); else selected.delete(v);
        applyFilter();
      });
    });
    applyFilter();
  }

  // ---- idea board: native drag-to-reorder -----------------------------
  // The whole note is draggable; while a note is in edit mode (its checkbox is
  // checked) dragging is suppressed so the textarea stays usable. On drop the new
  // left-to-right order is POSTed to /ideas/reorder — the DOM is already in that
  // order, so no swap-back is needed. Re-binds when the widget re-renders (guarded
  // by board.dataset.dragReady so a todo-side swap doesn't double-bind).
  function afterElement(board, x, y) {
    var els = Array.prototype.slice.call(board.querySelectorAll('.note[draggable=true]:not(.note--dragging)'));
    for (var i = 0; i < els.length; i++) {
      var b = els[i].getBoundingClientRect();
      if (y < b.top) return els[i];                       // pointer is above this row
      if (y <= b.bottom && x < b.left + b.width / 2) return els[i]; // same row, left of centre
    }
    return null;                                          // past everything → append
  }
  function persistOrder(board) {
    var ids = Array.prototype.slice.call(board.querySelectorAll('.note[draggable=true]'))
      .map(function (n) { return n.dataset.id; }).filter(Boolean);
    if (!ids.length) return;
    fetch('/ideas/reorder', {
      method: 'POST',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      body: 'ids=' + encodeURIComponent(ids.join(','))
    });
  }
  function initIdeas() {
    var board = document.querySelector('#ideaWidget .ideaboard');
    if (!board || board.dataset.dragReady) return;
    board.dataset.dragReady = '1';
    var dragEl = null;
    board.addEventListener('dragstart', function (e) {
      var n = e.target.closest && e.target.closest('.note[draggable=true]');
      if (!n) return;
      var chk = n.querySelector('.idea__edit-chk');
      if (chk && chk.checked) { e.preventDefault(); return; } // don't drag while editing
      dragEl = n;
      setTimeout(function () { n.classList.add('note--dragging'); }, 0);
      if (e.dataTransfer) {
        e.dataTransfer.effectAllowed = 'move';
        try { e.dataTransfer.setData('text/plain', n.dataset.id || ''); } catch (_) {}
      }
    });
    board.addEventListener('dragover', function (e) {
      if (!dragEl) return;
      e.preventDefault();
      if (e.dataTransfer) e.dataTransfer.dropEffect = 'move';
      var before = afterElement(board, e.clientX, e.clientY);
      if (before) board.insertBefore(dragEl, before);
      else board.appendChild(dragEl);
    });
    board.addEventListener('dragend', function () {
      if (!dragEl) return;
      dragEl.classList.remove('note--dragging');
      dragEl = null;
      persistOrder(board);
    });
  }

  function initMaze() {
    initFilter();
    applyTagColors();
    initIdeas();
  }
  document.addEventListener('DOMContentLoaded', initMaze);
  document.body.addEventListener('htmx:afterSwap', function (e) {
    if (document.getElementById('todoWidget')) initMaze();
  });
})();

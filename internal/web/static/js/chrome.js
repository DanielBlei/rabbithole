// Page chrome/main menu: opening the settings dialog, the popmenu filters, and
// the left-edge side menu.

// Settings opens from two places — the topbar gear and the side menu's
// `$ settings` — so the trigger is an attribute, not an id. Only the opening
// lives here: modal.js has the dialog registered as its bottom layer and owns
// closing it, the scroll lock, the focus trap and returning focus to whatever
// opened it.
(function(){
  var dialog = document.getElementById('settingsModal');
  if (!dialog) return;
  document.addEventListener('click', function(e){
    if (!e.target.closest('[data-stg-open]')) return;
    e.preventDefault();
    dialog.hidden = false;
  });
})();

// The cursor switch at the head of the side menu. The prompt cursors are the
// chrome's one piece of constant motion, and constant motion in the corner of
// the eye is a reasonable thing to want gone — so off takes them away rather
// than only stilling them. The switch keeps a cursor of its own, so there is
// always something to press to bring them back.
// Stored like the other chrome prefs and applied pre-paint by fonts-preload.js.
(function(){
  var root = document.documentElement;
  function sync(){
    var off = root.getAttribute('data-blink') === 'off';
    document.querySelectorAll('[data-blink-toggle]').forEach(function(btn){
      btn.setAttribute('aria-label', off ? 'Show the cursors' : 'Hide the cursors');
      btn.setAttribute('aria-pressed', off ? 'true' : 'false');
    });
  }
  document.addEventListener('click', function(e){
    if (!e.target.closest('[data-blink-toggle]')) return;
    if (root.getAttribute('data-blink') === 'off'){
      root.removeAttribute('data-blink'); localStorage.removeItem('blink');
    } else {
      root.setAttribute('data-blink', 'off'); localStorage.setItem('blink', 'off');
    }
    sync();
  });
  sync();
})();

// Popmenus (<details class="popmenu">: the feed's Source/Tags/View/Sort, the
// Maze tag filter) close on a click outside or Escape. A <details> only
// toggles from its own summary, so without this an opened panel stays up
// until you click that summary again. Delegated on document, so menus htmx
// swaps in are covered too.
(function(){
  function closeAll(except){
    document.querySelectorAll('details.popmenu[open]').forEach(function(menu){
      if (menu !== except) menu.open = false;
    });
  }
  document.addEventListener('click', function(e){
    // The clicked menu stays open — chips inside it are the point, and its
    // own summary already toggles. Any other open menu closes, so two can't
    // overlap.
    closeAll(e.target.closest('details.popmenu'));
  });
  document.addEventListener('keydown', function(e){
    if (e.key === 'Escape') closeAll(null);
  });
})();

// Side menu, in two modes.
//
// Kept open (the default): the column stays out and nothing dismisses it but
// the chevron inside it. Summoned: it is off-screen; hovering the left-edge hot
// zone slides it out and leaving puts it back, while the 3-line tab or the
// chevron keeps it out for good. The page holds still either way — the menu
// lies over it, never pushes it. The choice lives in localStorage and is
// applied pre-paint by fonts-preload.js.
//
// The page never moves for the menu, so below the width where the menu would
// lie on top of the content the CSS forces the summoned behaviour whatever the
// stored choice is — "kept open" here has to agree with that threshold, not
// only with the attribute.
(function(){
  var rail = document.getElementById('navRail');
  var hot = document.getElementById('navHot');
  if (!rail || !hot) return;
  var root = document.documentElement;
  var wide = window.matchMedia('(min-width:1680px)');

  function isDocked(){ return wide.matches && !root.hasAttribute('data-rail'); }

  var closeT = null, openT = null;
  function open(){
    clearTimeout(closeT); clearTimeout(openT);
    rail.classList.add('open');
    document.body.classList.add('nav-open');
  }
  function closeNow(){
    clearTimeout(closeT); clearTimeout(openT);
    rail.classList.remove('open');
    document.body.classList.remove('nav-open');
  }
  function closeSoon(){
    if (isDocked()) return;
    clearTimeout(closeT);
    closeT = setTimeout(closeNow, 240);
  }
  // The hot zone is a 12px strip, so opening the instant it is touched means
  // the menu flies out at anything crossing the left edge on its way past. A
  // short dwell asks for intent. The tab and the open rail stay immediate.
  function openSoon(){
    if (isDocked()) return;
    clearTimeout(openT);
    openT = setTimeout(open, 110);
  }

  function setDocked(docked){
    if (docked){ root.removeAttribute('data-rail'); localStorage.removeItem('rail'); }
    else { root.setAttribute('data-rail', 'off'); localStorage.setItem('rail', 'off'); }
    // Docking makes the temporary reveal meaningless, and undocking must not
    // leave the column stranded on screen with the page already closed up.
    closeNow();
    syncPin();
  }
  function syncPin(){
    var pin = rail.querySelector('[data-rail-toggle]');
    if (pin) pin.setAttribute('aria-label', isDocked() ? 'Hide the menu' : 'Keep the menu open');
  }

  hot.addEventListener('mouseenter', openSoon);
  hot.addEventListener('mouseleave', function(){ clearTimeout(openT); });
  rail.addEventListener('mouseenter', function(){ if (!isDocked()) open(); });
  rail.addEventListener('mouseleave', closeSoon);
  rail.addEventListener('click', function(e){
    // Docked, picking a page is just navigation — there is nothing to put away.
    if (e.target.closest('.navrail__item') && !isDocked()) closeNow();
  });
  document.addEventListener('click', function(e){
    if (e.target.closest('[data-rail-toggle]')){ setDocked(!isDocked()); return; }
    // The tab only exists while collapsed, and clicking it pins the menu back.
    if (e.target.closest('#navTab')){ setDocked(true); return; }
    if (!isDocked() && rail.classList.contains('open') && !rail.contains(e.target)) closeNow();
  });
  document.addEventListener('keydown', function(e){
    if (e.key === 'Escape' && !isDocked()) closeNow();
  });
  // Crossing the width threshold changes which mode is in force, so the
  // temporary reveal must not survive the crossing.
  wide.addEventListener('change', function(){ closeNow(); syncPin(); });
  syncPin();
})();

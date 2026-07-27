// Page chrome/main menu: the settings dropdown, the popmenu filters, and the left-edge side menu.

(function(){
  var btn = document.getElementById('settingsToggle');
  var panel = document.getElementById('settingsPanel');

  function close(){
    panel.hidden = true;
    btn.setAttribute('aria-expanded', 'false');
  }
  function open(){
    panel.hidden = false;
    btn.setAttribute('aria-expanded', 'true');
  }

  btn.addEventListener('click', function(e){
    e.stopPropagation();
    if (panel.hidden) { open(); } else { close(); }
  });
  document.addEventListener('click', function(e){
    if (!panel.hidden && !panel.contains(e.target) && e.target !== btn) close();
  });
  document.addEventListener('keydown', function(e){
    if (e.key === 'Escape') close();
  });
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

// Side menu: hovering the left-edge hot zone (or clicking the 3-line tab)
// slides the docked column out; it retracts on mouse-leave, on a click
// elsewhere, or after picking an item. The tab is OOB-replaced by ingest
// responses, so its click is delegated on document rather than bound.
(function(){
  var rail = document.getElementById('navRail');
  var hot = document.getElementById('navHot');
  if (!rail || !hot) return;
  var closeT = null;
  function open(){
    clearTimeout(closeT);
    rail.classList.add('open');
    document.body.classList.add('nav-open');
  }
  function closeNow(){
    clearTimeout(closeT);
    rail.classList.remove('open');
    document.body.classList.remove('nav-open');
  }
  function closeSoon(){
    clearTimeout(closeT);
    closeT = setTimeout(closeNow, 240);
  }
  hot.addEventListener('mouseenter', open);
  rail.addEventListener('mouseenter', open);
  rail.addEventListener('mouseleave', closeSoon);
  rail.addEventListener('click', function(e){
    if (e.target.closest('.navrail__item')) closeNow();
  });
  document.addEventListener('click', function(e){
    if (e.target.closest('#navTab')) { open(); return; }
    if (rail.classList.contains('open') && !rail.contains(e.target)) closeNow();
  });
  document.addEventListener('keydown', function(e){
    if (e.key === 'Escape') closeNow();
  });
})();

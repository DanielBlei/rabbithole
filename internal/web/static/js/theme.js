// Chrome layout pref, edited from Settings → Theme. Not the palette — the
// palette is fixed — but the shape the chrome is drawn in: the bar, the side
// menu and the feed's density.
//
// Same contract as the font prefs it sits beside: the value lives in
// localStorage, is reflected as data-theme on <html>, and the stylesheet's
// layout layer resolves it. The default is stored as absence, so a user who
// never opens this tab carries no key and no attribute. fonts-preload.js has
// already applied it pre-paint; this owns the UI wiring.
//
// Nothing here names the layouts: a card in layout.html carries the value and
// the stylesheet's layout layer resolves it, so a new one needs no edit here.
(function(){
  var DEFAULT = 'default';
  var root = document.documentElement;
  var picks = document.querySelectorAll('[data-theme-pick]');
  if (!picks.length) return;

  function get(){ return localStorage.getItem('theme') || DEFAULT; }
  function apply(){
    var value = get();
    if (value === DEFAULT) root.removeAttribute('data-theme');
    else root.setAttribute('data-theme', value);
  }
  function syncControls(){
    var value = get();
    picks.forEach(function(pick){ pick.checked = pick.value === value; });
  }

  picks.forEach(function(pick){
    pick.addEventListener('change', function(){
      if (!pick.checked) return;
      if (pick.value === DEFAULT) localStorage.removeItem('theme');
      else localStorage.setItem('theme', pick.value);
      apply();
    });
  });

  apply();
  syncControls();
})();

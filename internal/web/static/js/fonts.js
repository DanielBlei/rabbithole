// Font prefs: Interface / Titles / Text faces + sizes, edited from
// Settings → Theme → Appearance. Pure presentation in localStorage
// (font.ui/uiSize/title/body/titleSize/bodySize), reflected as
// data-fu/fus/ft/fb/fts/fbs attrs on <html> that the stylesheet's user type
// layer resolves. Attributes live on the root element, so they survive htmx
// swaps — no afterSwap re-init needed. fonts-preload.js already applied stored
// prefs pre-paint; this owns the UI wiring.
(function(){
  var ATTRS    = {ui:'data-fu', uiSize:'data-fus',
                  title:'data-ft', body:'data-fb', titleSize:'data-fts', bodySize:'data-fbs'};
  var DEFAULTS = {ui:'mono', uiSize:'m', title:'mono', body:'mono', titleSize:'m', bodySize:'m'};
  // What a theme starts from until the reader picks otherwise; mirrors the
  // :not([data-fu]) / :not([data-ft]) / :not([data-fb]) rules in style.css and
  // the per-layout interface ladder beside them. Minimal opens on Small because
  // its ladder is anchored to its own density, not Default's.
  var THEME_FACES = {minimal:{ui:'plex', uiSize:'s', title:'grotesk', body:'plex'}};
  var FACE_KEYS = ['ui','title','body'];
  var FACES = ['mono','inter','plex','manrope','grotesk','serif'];
  // The chrome gets two of the six. It is dense small type in fixed pills and
  // rails, and the pick there is about density rather than decoration.
  var UI_FACES = ['mono','plex'];
  var NAMES = {mono:'Plex Mono', inter:'Inter', plex:'Plex Sans', manrope:'Manrope', grotesk:'Space Grotesk', serif:'Source Serif'};
  // Three steps and no more: the browser's own zoom covers anything beyond.
  var SIZES = ['s','m','l'];
  // One ladder of names, read at an offset. The step a layout anchors on is
  // Default and the others are named by which way they move from it, so a name
  // says where the step stands rather than how big it is in the abstract.
  // Titles and Text anchor in the middle under both layouts; the interface
  // ladder anchors at the bottom under Minimal, which is already the compact
  // one, so it climbs instead of dropping.
  var SIZE_STEPS = ['Smaller', 'Default', 'Bigger', 'Biggest'];
  var root = document.documentElement;

  function isFace(k){ return FACE_KEYS.indexOf(k) >= 0; }
  // The options a key offers — not one list for all faces, since the chrome's is
  // shorter. Every read of a stored value goes through here too, so a pref that
  // is no longer on offer reads as unset rather than sticking.
  function optionsFor(k){ return k === 'ui' ? UI_FACES : isFace(k) ? FACES : SIZES; }
  // The names this key's ladder wears, anchored on the step the layout starts
  // from. Depends on the theme, so it is computed rather than stored.
  function sizeNames(k){
    var at = SIZES.indexOf(fallback(k));
    return SIZES.map(function(_, i){ return SIZE_STEPS[i - at + 1]; });
  }
  function nameOf(k, v){
    if (isFace(k)) return NAMES[v] || NAMES.mono;
    var i = SIZES.indexOf(v);
    return sizeNames(k)[i < 0 ? SIZES.indexOf(fallback(k)) : i];
  }
  function fallback(k){
    var faces = THEME_FACES[root.getAttribute('data-theme')];
    return (faces && faces[k]) || DEFAULTS[k];
  }
  // A stored value that is no longer one of the options (a size step that was
  // retired) counts as unset.
  function stored(k){
    var v = localStorage.getItem('font.'+k);
    return optionsFor(k).indexOf(v) >= 0 ? v : null;
  }
  function get(k){ return stored(k) || fallback(k); }
  function set(k, v){
    // A pick equal to what the theme would use anyway is no pick: it is stored
    // as absence, so the value keeps following the theme and unset users carry
    // no keys or attrs. Anything else is a choice, and it travels across themes.
    if (v === fallback(k)) localStorage.removeItem('font.'+k);
    else localStorage.setItem('font.'+k, v);
    apply(); syncControls();
  }
  function apply(){
    for (var k in ATTRS){
      var v = stored(k);
      if (v) root.setAttribute(ATTRS[k], v);
      else root.removeAttribute(ATTRS[k]);
    }
  }

  function syncControls(){
    document.querySelectorAll('[data-font-step]').forEach(function(step){
      var k = step.getAttribute('data-font-step');
      var v = get(k);
      var name = step.querySelector('[data-font-name]');
      name.textContent = nameOf(k, v);
      // A face name is drawn in its own face, so the stepper is a sample too.
      name.className = 'font-step__name' + (isFace(k) ? ' ff-' + (NAMES[v] ? v : 'mono') : '');
      var options = optionsFor(k);
      var at = options.indexOf(v);
      // A size ladder has ends, and the arrow at one goes quiet rather than
      // jumping to the far end: the labels on them read Smaller and Bigger.
      step.querySelector('[data-font-prev]').disabled = !isFace(k) && at === 0;
      step.querySelector('[data-font-next]').disabled = !isFace(k) && at === options.length - 1;
      step.querySelectorAll('[data-font-dots] i').forEach(function(dot, i){
        dot.classList.toggle('is-on', i === at);
      });
    });
  }

  document.querySelectorAll('[data-font-step]').forEach(function(step){
    var k = step.getAttribute('data-font-step');
    var options = optionsFor(k);
    // The dot strip is as long as the list it tracks, which is two for the
    // interface face and six for the other two.
    var dots = step.querySelector('[data-font-dots]');
    options.forEach(function(){ dots.appendChild(document.createElement('i')); });
    // A face list is a cycle, so its ends join and two options are walked in
    // either direction. A size ladder is an order, so it stops at its ends.
    function walk(by){
      var at = options.indexOf(get(k)) + by;
      if (isFace(k)) at = (at + options.length) % options.length;
      else if (at < 0 || at >= options.length) return;
      set(k, options[at]);
    }
    step.querySelector('[data-font-prev]').addEventListener('click', function(){ walk(-1); });
    step.querySelector('[data-font-next]').addEventListener('click', function(){ walk(1); });
  });

  // Switching theme changes which face is unset, so the buttons follow it.
  new MutationObserver(syncControls).observe(root, {attributes:true, attributeFilter:['data-theme']});

  apply();
  syncControls();
})();

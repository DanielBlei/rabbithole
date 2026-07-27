// Font prefs: Titles / Text faces + sizes, edited from Settings → Fonts.
// Pure presentation in localStorage (font.title/body/titleSize/bodySize),
// reflected as data-ft/fb/fts/fbs attrs on <html> that the stylesheet's
// user type layer resolves. Attributes live on the root element, so they
// survive htmx swaps — no afterSwap re-init needed. fonts-preload.js
// already applied stored prefs pre-paint; this owns the UI wiring.
(function(){
  var ATTRS    = {title:'data-ft', body:'data-fb', titleSize:'data-fts', bodySize:'data-fbs'};
  var DEFAULTS = {title:'mono', body:'mono', titleSize:'m', bodySize:'m'};
  var SIZES    = ['s','m','l','xl','xxl'];
  // stepper cycle order + display names; the name renders in its own face
  var FACES = ['mono','inter','plex','manrope','grotesk','serif'];
  var NAMES = {mono:'Plex Mono', inter:'Inter', plex:'Plex Sans', manrope:'Manrope', grotesk:'Space Grotesk', serif:'Source Serif'};
  var root = document.documentElement;

  function get(k){ return localStorage.getItem('font.'+k) || DEFAULTS[k]; }
  function set(k, v){
    // defaults are stored as absence, so unset users carry no keys or attrs
    if (v === DEFAULTS[k]) localStorage.removeItem('font.'+k);
    else localStorage.setItem('font.'+k, v);
    apply(); syncControls();
  }
  function apply(){
    for (var k in ATTRS){
      var v = get(k);
      if (v === DEFAULTS[k]) root.removeAttribute(ATTRS[k]);
      else root.setAttribute(ATTRS[k], v);
    }
  }

  function syncControls(){
    document.querySelectorAll('.font-step').forEach(function(st){
      var face = get(st.getAttribute('data-font-step'));
      var name = st.querySelector('[data-font-name]');
      name.textContent = NAMES[face] || NAMES.mono;
      name.className = 'font-step__name ff-' + (NAMES[face] ? face : 'mono');
      var i = FACES.indexOf(face); if (i < 0) i = 0;
      st.querySelectorAll('[data-font-dots] i').forEach(function(dot, j){
        dot.classList.toggle('is-on', j === i);
      });
    });
    document.querySelectorAll('[data-font-range]').forEach(function(range){
      var i = SIZES.indexOf(get(range.getAttribute('data-font-range')));
      range.value = i < 0 ? SIZES.indexOf('m') : i;
    });
  }

  document.querySelectorAll('.font-step').forEach(function(st){
    var key  = st.getAttribute('data-font-step');
    var dots = st.querySelector('[data-font-dots]');
    FACES.forEach(function(){ dots.appendChild(document.createElement('i')); });
    function step(dir){
      var i = FACES.indexOf(get(key)); if (i < 0) i = 0;
      set(key, FACES[(i + dir + FACES.length) % FACES.length]);
    }
    st.querySelector('[data-font-prev]').addEventListener('click', function(){ step(-1); });
    st.querySelector('[data-font-next]').addEventListener('click', function(){ step(1); });
  });
  document.querySelectorAll('[data-font-range]').forEach(function(range){
    range.addEventListener('input', function(){
      set(range.getAttribute('data-font-range'), SIZES[parseInt(range.value, 10)] || 'm');
    });
  });

  apply();
  syncControls();
})();

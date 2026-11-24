// Apply the stored font and layout prefs before first paint so there's no flash
// of the default face or the default chrome, and preload the chosen faces' files
// so they are not fetched only after the stylesheet resolves.
// Loaded blocking (no defer) from <head> for that reason; fonts.js and theme.js
// own the actual UI wiring.
(function(){
  var m={'ui':'data-fu','uiSize':'data-fus','title':'data-ft','body':'data-fb',
         'titleSize':'data-fts','bodySize':'data-fbs'};
  // Checked against the same option lists fonts.js offers. A value no rule knows
  // would be worse than no value: the stylesheet falls back to the base face,
  // and the attribute's mere presence switches off the :not([data-fu]) rules a
  // layout uses for its own defaults — so a stale pref would make Minimal paint
  // mono chrome. fonts.js repairs that on the next frame, but the login page
  // loads this file alone and never gets the chance.
  var ok={'ui':['mono','plex'],'uiSize':['s','m','l'],
          'title':['mono','inter','plex','manrope','grotesk','serif'],
          'titleSize':['s','m','l'],'bodySize':['s','m','l']};
  ok['body']=ok['title'];
  // One failing read (storage blocked) must not skip the rest.
  function get(key){ try { return localStorage.getItem(key); } catch (e) { return null; } }
  // Regular for interface and text, semibold for titles; Source Serif ships no
  // regular, so its text weight is 500.
  var file={'mono':'ibm-plex-mono-v20','plex':'ibm-plex-sans-v23','inter':'inter-v20',
            'manrope':'manrope-v20','grotesk':'space-grotesk-v22','serif':'source-serif-4-v14'};
  function preload(face, w){
    var f=file[face]; if (!f) return;
    if (w==='regular' && face==='serif') w='500';
    var l=document.createElement('link');
    l.rel='preload'; l.as='font'; l.type='font/woff2'; l.crossOrigin='anonymous';
    l.href='/static/fonts/'+f+'-latin_latin-ext-'+w+'.woff2';
    document.head.appendChild(l);
  }
  for (var k in m){
    var v=get('font.'+k);
    if (v && ok[k].indexOf(v) >= 0){
      document.documentElement.setAttribute(m[k], v);
      if (k==='ui' || k==='body') preload(v, 'regular');
      else if (k==='title') preload(v, '600');
    }
  }
  // The chrome layout and whether the side menu is docked. Absence is the
  // default in both cases, so only a non-default choice lands on the element —
  // and the menu's state has to be here, not in chrome.js, or the page indents
  // after the first paint and everything jumps sideways.
  var t=get('theme');
  if (t) document.documentElement.setAttribute('data-theme', t);
  // The layout the page opened in, which nothing changes afterwards. The
  // login's blurred background keys on it, so a pick on the first-run card
  // restyles the card while what sits behind it holds still.
  if (t) document.documentElement.setAttribute('data-ghost', t);
  if (get('rail')==='off') document.documentElement.setAttribute('data-rail','off');
  if (get('navx')==='off') document.documentElement.setAttribute('data-navx','off');
  if (get('blink')==='off') document.documentElement.setAttribute('data-blink','off');
  // Where the weather shows, reproduced from the same keys weather.js writes.
  // Here for two reasons, both of them things you would otherwise watch
  // happen: the side menu is drawn on the first paint, so a box appearing in
  // it afterwards shoves the config block down the column; and the inline
  // shape replaces a tile the server has already rendered, so without this you
  // see the placeholder swap for the weather on every load. weather.js owns
  // these values and the migration; this only reads them.
  var on = get('weather.show') !== '0';
  var mode = get('weather.mode') || 'subbar';
  var feed = get('weather.feed');
  var maze = get('weather.maze');
  var side = get('weather.side') || 'off';
  // A pref written before the two axes existed: one value that named both the
  // shape and the place. Read it the old way rather than guessing, and leave
  // the rewriting to weather.js.
  if (mode.indexOf('sidebar') === 0){
    side = mode === 'sidebar-down' ? 'down' : 'up';
    mode = 'subbar'; feed = '0'; maze = '0';
  } else if (mode === 'top' || mode === 'bottom' || mode.indexOf('rail') === 0){
    mode = 'full';
  }
  var onFeed = on && feed !== '0', onMaze = on && maze !== '0';
  if (on && (onFeed || onMaze)){
    document.documentElement.setAttribute('data-wx-shape', mode);
    if (onFeed) document.documentElement.setAttribute('data-wx-feed', '1');
    if (onMaze) document.documentElement.setAttribute('data-wx-maze', '1');
  }
  if (on && side !== 'off') document.documentElement.setAttribute('data-wx-place', side);
})();

// Apply the stored font and layout prefs before first paint so there's no flash
// of the default face or the default chrome.
// Loaded blocking (no defer) from <head> for that reason; fonts.js and theme.js
// own the actual UI wiring.
(function(){
  var m={'title':'data-ft','body':'data-fb','titleSize':'data-fts','bodySize':'data-fbs'};
  for (var k in m){
    var v=localStorage.getItem('font.'+k);
    if (v) document.documentElement.setAttribute(m[k], v);
  }
  // The chrome layout and whether the side menu is docked. Absence is the
  // default in both cases, so only a non-default choice lands on the element —
  // and the menu's state has to be here, not in chrome.js, or the page indents
  // after the first paint and everything jumps sideways.
  var t=localStorage.getItem('theme');
  if (t) document.documentElement.setAttribute('data-theme', t);
  if (localStorage.getItem('rail')==='off') document.documentElement.setAttribute('data-rail','off');
  if (localStorage.getItem('blink')==='off') document.documentElement.setAttribute('data-blink','off');
  // Where the weather shows, reproduced from the same keys weather.js writes.
  // Here for two reasons, both of them things you would otherwise watch
  // happen: the side menu is drawn on the first paint, so a box appearing in
  // it afterwards shoves the config block down the column; and the inline
  // shape replaces a tile the server has already rendered, so without this you
  // see the placeholder swap for the weather on every load. weather.js owns
  // these values and the migration; this only reads them.
  var on = localStorage.getItem('weather.show') !== '0';
  var mode = localStorage.getItem('weather.mode') || 'subbar';
  var feed = localStorage.getItem('weather.feed');
  var maze = localStorage.getItem('weather.maze');
  var side = localStorage.getItem('weather.side') || 'off';
  // A pref written before the two axes existed: one value that named both the
  // shape and the place. Read it the old way rather than guessing, and leave
  // the rewriting to weather.js.
  if (mode.indexOf('sidebar') === 0){
    side = mode === 'sidebar-down' ? 'down' : 'up';
    mode = 'subbar'; feed = '0'; maze = '0';
  } else if (mode === 'top' || mode === 'bottom' || mode.indexOf('rail') === 0){
    mode = 'full';
  }
  var onFeed = on && feed === '1', onMaze = on && maze !== '0';
  if (on && (onFeed || onMaze)){
    document.documentElement.setAttribute('data-wx-shape', mode);
    if (onFeed) document.documentElement.setAttribute('data-wx-feed', '1');
    if (onMaze) document.documentElement.setAttribute('data-wx-maze', '1');
  }
  if (on && side !== 'off') document.documentElement.setAttribute('data-wx-place', side);
})();

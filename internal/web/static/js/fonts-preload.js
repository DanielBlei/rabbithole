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
})();

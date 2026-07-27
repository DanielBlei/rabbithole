// Apply the stored font prefs before first paint so there's no flash of the default face. 
// Loaded blocking (no defer) from <head> for that reason; fonts.js owns the actual UI wiring.
(function(){
  var m={'title':'data-ft','body':'data-fb','titleSize':'data-fts','bodySize':'data-fbs'};
  for (var k in m){
    var v=localStorage.getItem('font.'+k);
    if (v) document.documentElement.setAttribute(m[k], v);
  }
})();

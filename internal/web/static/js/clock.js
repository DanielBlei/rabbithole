// Topbar clock: a live local-time + date readout, formatted entirely client-
// side and ticked every second. Format prefs (24h, seconds, date style) live
// in localStorage and are edited from the settings panel — pure presentation,
// so no server round-trip and nothing touches the store or config.
(function(){
  var clock = document.getElementById('clock');
  if (!clock) return;
  var timeEl = clock.querySelector('.clock__time');
  var dateEl = clock.querySelector('.clock__date');

  var WD = ['Sunday','Monday','Tuesday','Wednesday','Thursday','Friday','Saturday'];
  var MO = ['January','February','March','April','May','June','July','August','September','October','November','December'];
  function pad(n){ return n < 10 ? '0'+n : ''+n; }
  function ord(n){ var s=['th','st','nd','rd'], v=n%100; return n + (s[(v-20)%10] || s[v] || s[0]); }

  function get(k, def){
    var v = localStorage.getItem('clock.'+k);
    if (v === null) return def;
    return typeof def === 'boolean' ? v === '1' : v;
  }
  function set(k, val){
    localStorage.setItem('clock.'+k, typeof val === 'boolean' ? (val?'1':'0') : val);
  }

  var prefs = {
    time24:  get('time24', true),
    seconds: get('seconds', false),
    dateFmt: get('dateFmt', 'medium')
  };
  // Date formats ordered compact→verbose so the slider reads left-to-right.
  var FMTS = ['numeric','short','medium','long','ordinal','hidden'];

  function fmtTime(d){
    var h = d.getHours(), ap = '';
    if (!prefs.time24){ ap = h < 12 ? ' AM' : ' PM'; h = h % 12 || 12; }
    var out = (prefs.time24 ? pad(h) : h) + ':' + pad(d.getMinutes());
    if (prefs.seconds) out += ':' + pad(d.getSeconds());
    return out + ap;
  }
  function fmtDate(d){
    if (prefs.dateFmt === 'hidden') return '';
    var wS = WD[d.getDay()].slice(0,3), wL = WD[d.getDay()];
    var mS = MO[d.getMonth()].slice(0,3), mL = MO[d.getMonth()];
    var day = d.getDate(), yr = d.getFullYear();
    switch (prefs.dateFmt){
      case 'medium':  return wS+' '+day+' '+mS+' '+yr;
      case 'long':    return wL+' '+day+' '+mL+' '+yr;
      case 'ordinal': return wL+' '+ord(day)+' '+mL+' '+yr;
      case 'numeric': return pad(day)+'/'+pad(d.getMonth()+1)+'/'+yr;
      default:        return wS+' '+day+' '+mS; // short
    }
  }
  function render(){
    var d = new Date();
    timeEl.textContent = fmtTime(d);
    var ds = fmtDate(d);
    dateEl.textContent = ds;
    dateEl.hidden = !ds;
  }

  // Settings controls: two toggle buttons + a date-format slider, styled like
  // the page's other pills (no native select/checkbox). syncControls reflects
  // current prefs back onto the controls; clicks/slides write prefs live.
  var toggles = document.querySelectorAll('[data-clk-toggle]');
  var range   = document.querySelector('[data-clk-range="dateFmt"]');
  var preview = document.querySelector('[data-clk-preview]');

  function syncControls(){
    toggles.forEach(function(btn){
      var key = btn.getAttribute('data-clk-toggle');
      btn.classList.toggle('is-on', !!prefs[key]);
      btn.setAttribute('aria-pressed', prefs[key] ? 'true' : 'false');
      if (key === 'time24') btn.textContent = prefs.time24 ? '24-hour' : 'AM / PM';
    });
    if (range){
      var i = FMTS.indexOf(prefs.dateFmt); if (i < 0) i = FMTS.indexOf('medium');
      range.value = i;
    }
    if (preview) preview.textContent = prefs.dateFmt === 'hidden' ? 'No date' : fmtDate(new Date());
  }

  toggles.forEach(function(btn){
    btn.addEventListener('click', function(){
      var key = btn.getAttribute('data-clk-toggle');
      prefs[key] = !prefs[key]; set(key, prefs[key]);
      syncControls(); render();
    });
  });
  if (range){
    range.addEventListener('input', function(){
      prefs.dateFmt = FMTS[parseInt(range.value, 10)] || 'medium';
      set('dateFmt', prefs.dateFmt);
      syncControls(); render();
    });
  }

  syncControls();
  render();
  setInterval(render, 1000);
})();

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
  // Date formats ordered compact→verbose, which is the order the settings
  // tiles read in.
  var FMTS = ['numeric','short','medium','long','ordinal','hidden'];

  // time24 defaults to the current pref; the settings tiles pass their own so
  // each can show the clock as that setting would render it.
  function fmtTime(d, time24){
    if (time24 === undefined) time24 = prefs.time24;
    var h = d.getHours(), ap = '';
    if (!time24){ ap = h < 12 ? ' AM' : ' PM'; h = h % 12 || 12; }
    var out = (time24 ? pad(h) : h) + ':' + pad(d.getMinutes());
    if (prefs.seconds) out += ':' + pad(d.getSeconds());
    return out + ap;
  }
  // fmt defaults to the current pref; the settings tiles pass their own so each
  // can render today's date in the style it offers.
  function fmtDate(d, fmt){
    fmt = fmt || prefs.dateFmt;
    if (fmt === 'hidden') return '';
    var wS = WD[d.getDay()].slice(0,3), wL = WD[d.getDay()];
    var mS = MO[d.getMonth()].slice(0,3), mL = MO[d.getMonth()];
    var day = d.getDate(), yr = d.getFullYear();
    switch (fmt){
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

  // Settings controls: the clock tiles, the seconds switch and the date tiles.
  // syncControls reflects current prefs back onto them; a click writes them
  // live. The two tile groups preview themselves rather than naming a state.
  var toggles  = document.querySelectorAll('[data-clk-toggle]');
  var fmtOpts  = document.querySelectorAll('[data-clk-fmt]');
  var samples  = document.querySelectorAll('[data-clk-sample]');
  var hourOpts = document.querySelectorAll('[data-clk-hours]');
  var hourSamples = document.querySelectorAll('[data-clk-tsample]');

  // The clock tiles tick with the clock itself — unlike the date samples, what
  // they show changes every second.
  function fillHourSamples(){
    var now = new Date();
    hourSamples.forEach(function(el){
      el.textContent = fmtTime(now, el.getAttribute('data-clk-tsample') === '24');
    });
  }

  // Each tile carries today's date in the style it offers, so the choice is
  // made by reading rather than by trying each one.
  function fillSamples(){
    var now = new Date();
    samples.forEach(function(el){
      var fmt = el.getAttribute('data-clk-sample');
      el.textContent = fmt === 'hidden' ? 'No date' : fmtDate(now, fmt);
    });
  }

  function syncControls(){
    toggles.forEach(function(btn){
      var key = btn.getAttribute('data-clk-toggle');
      btn.classList.toggle('is-on', !!prefs[key]);
      btn.setAttribute('aria-pressed', prefs[key] ? 'true' : 'false');
      if (key === 'seconds') btn.textContent = prefs.seconds ? 'On' : 'Off';
    });
    fmtOpts.forEach(function(opt){ opt.checked = opt.value === prefs.dateFmt; });
    hourOpts.forEach(function(opt){ opt.checked = (opt.value === '24') === !!prefs.time24; });
  }

  toggles.forEach(function(btn){
    btn.addEventListener('click', function(){
      var key = btn.getAttribute('data-clk-toggle');
      prefs[key] = !prefs[key]; set(key, prefs[key]);
      // Seconds changes what the clock tiles show, not just the topbar.
      syncControls(); render(); fillHourSamples();
    });
  });
  fmtOpts.forEach(function(opt){
    opt.addEventListener('change', function(){
      if (!opt.checked) return;
      prefs.dateFmt = FMTS.indexOf(opt.value) < 0 ? 'medium' : opt.value;
      set('dateFmt', prefs.dateFmt);
      render();
    });
  });
  hourOpts.forEach(function(opt){
    opt.addEventListener('change', function(){
      if (!opt.checked) return;
      prefs.time24 = opt.value === '24';
      set('time24', prefs.time24);
      render(); fillHourSamples();
    });
  });

  syncControls();
  fillSamples();
  fillHourSamples();
  render();
  // The samples are a day's worth of text, so they only need rewriting when the
  // day turns over — not on every tick with the clock.
  var sampleDay = new Date().getDate();
  setInterval(function(){
    render();
    fillHourSamples();
    var today = new Date().getDate();
    if (today !== sampleDay){ sampleDay = today; fillSamples(); }
  }, 1000);
})();

// Weather + pollen: fetched from Open-Meteo (current + hourly + daily) and
// its air-quality pollen API, keyed off a location set by geolocation or a
// typed place search. Everything — location, unit, chosen layout, show/hide
// — lives in localStorage (no server round-trip, nothing touches the store
// or config). One cached payload (30-min TTL) renders into whichever of the
// four Maze placements is active (sub-bar / inline chip / right or left
// rail); the CSS shows only the one matching .maze[data-wx-mode]. All of it
// no-ops when .maze isn't on the page (i.e. the Feed).
(function(){
  var CACHE_TTL = 30 * 60 * 1000;

  // layout stepper cycle order + display names
  var MODES = ['subbar','inline','rail-left','rail-right'];
  var MODE_NAMES = {'subbar':'Sub-bar','inline':'Inline','rail-left':'Rail left','rail-right':'Rail right'};

  function wxGet(k, def){
    var v = localStorage.getItem('weather.'+k);
    return v === null ? def : v;
  }
  function wxSet(k, v){
    localStorage.setItem('weather.'+k, typeof v === 'object' ? JSON.stringify(v) : String(v));
  }

  // WMO weather interpretation code → [emoji, description]
  var WX = {
    0:['☀','Clear sky'],
    1:['🌤','Mainly clear'],2:['⛅','Partly cloudy'],3:['☁','Overcast'],
    45:['🌫','Fog'],48:['🌫','Icy fog'],
    51:['🌦','Light drizzle'],53:['🌦','Drizzle'],55:['🌧','Heavy drizzle'],
    61:['🌦','Light rain'],63:['🌧','Rain'],65:['🌧','Heavy rain'],
    71:['🌨','Light snow'],73:['❄','Snow'],75:['❄','Heavy snow'],
    80:['🌦','Showers'],81:['🌧','Rain showers'],82:['⛈','Heavy showers'],
    95:['⛈','Thunderstorm'],96:['⛈','Hail storm'],99:['⛈','Heavy hail']
  };
  function wxFor(code){ return WX[code] || ['🌡','Unknown']; }

  var POLLEN_KEYS   = ['grass_pollen','birch_pollen','alder_pollen','mugwort_pollen','olive_pollen','ragweed_pollen'];
  var POLLEN_LABELS = {grass_pollen:'Grass',birch_pollen:'Birch',alder_pollen:'Alder',mugwort_pollen:'Mugwort',olive_pollen:'Olive',ragweed_pollen:'Ragweed'};

  function pollenLevel(v){
    if (v == null || v < 0) return null;
    if (v <= 10)  return {label:'Low',     sym:'○',cls:'pollen--low'};
    if (v <= 30)  return {label:'Moderate',sym:'◐',cls:'pollen--mod'};
    if (v <= 100) return {label:'High',    sym:'▲',cls:'pollen--high'};
    return              {label:'V.High',  sym:'⬆',cls:'pollen--vhigh'};
  }
  function unitSym(){ return wxGet('unit','C') === 'F' ? '°F' : '°C'; }
  // Hour label for the rail strip — 24h ("15h", default) or AM/PM ("3p"),
  // following the clock's 24h⇄AM/PM convention. The suffix also disambiguates
  // the hour from the temperature below it.
  function fmtHour(h){
    if (wxGet('time24','1') !== '0') return h + 'h';
    return (h % 12 || 12) + (h < 12 ? 'a' : 'p');
  }

  function fetchWeather(lat, lon){
    if (!lat || !lon) return;
    var tUnit = wxGet('unit','C') === 'F' ? 'fahrenheit' : 'celsius';
    var wxUrl = 'https://api.open-meteo.com/v1/forecast?latitude='+lat+'&longitude='+lon+
      '&current=temperature_2m,weather_code,apparent_temperature'+
      '&hourly=temperature_2m,weather_code'+
      '&daily=temperature_2m_max,temperature_2m_min,weather_code'+
      '&temperature_unit='+tUnit+'&timezone=auto&forecast_days=4';
    var poUrl = 'https://air-quality-api.open-meteo.com/v1/air-quality?latitude='+lat+'&longitude='+lon+
      '&current='+POLLEN_KEYS.join(',');
    Promise.all([fetch(wxUrl), fetch(poUrl)])
      .then(function(rs){ return Promise.all([rs[0].json(), rs[1].json()]); })
      .then(function(d){
        wxSet('data', d[0]);
        wxSet('pollen', d[1]);
        wxSet('fetchedAt', Date.now());
        renderWeather();
      })
      .catch(function(){});
  }

  function requestLocation(){
    if (!navigator.geolocation) return;
    navigator.geolocation.getCurrentPosition(function(p){
      wxSet('lat', p.coords.latitude);
      wxSet('lon', p.coords.longitude);
      wxSet('label', 'My location');
      wxSet('fetchedAt', '0');
      fetchWeather(p.coords.latitude, p.coords.longitude);
    });
  }

  function maybeRefresh(){
    var lat = parseFloat(wxGet('lat',''));
    var lon = parseFloat(wxGet('lon',''));
    if (!lat || !lon){ requestLocation(); return; }
    var age = Date.now() - parseInt(wxGet('fetchedAt','0'), 10);
    if (age > CACHE_TTL){ fetchWeather(lat, lon); return; }
    renderWeather();
  }

  // --- HTML builders, one per placement, off the cached payload ---
  // Allergens that have a reading, sorted strongest-first. The placements show
  // progressively more of this list: inline = top 1, sub-bar = top 2, rail = all.
  function pollenSorted(poData){
    if (!poData || !poData.current || wxGet('showPollen','1') === '0') return [];
    var cur = poData.current, arr = [];
    POLLEN_KEYS.forEach(function(k){
      var v = cur[k], lvl = pollenLevel(v);
      if (lvl) arr.push({name:POLLEN_LABELS[k], val:v, lvl:lvl});
    });
    arr.sort(function(a,b){ return b.val - a.val; });
    return arr;
  }
  function pollenPillsTop(poData, n){
    return pollenSorted(poData).slice(0, n).map(function(p){
      return '<span class="pollen-pill '+p.lvl.cls+'">'+p.name+' '+p.lvl.label+'</span>';
    }).join('');
  }
  function pollenRows(poData){
    return pollenSorted(poData).map(function(p){
      return '<div class="pr '+p.lvl.cls+'"><span>'+p.name+'</span><span>'+p.lvl.label+'</span></div>';
    }).join('');
  }
  // Rail = the full read-out: hero, a feels-like / today H-L line, an hourly
  // strip, a few days, and the complete pollen list. Expands vertically.
  function railHtml(wxData, poData){
    var cur = wxData.current, wx = wxFor(cur.weather_code), u = unitSym();
    var d = wxData.daily;
    var html = '<div class="wx-rail-lbl">'+(wxGet('label','Weather'))+' · now</div>'+
      '<div class="wx-rail-now"><span class="wx-icon">'+wx[0]+'</span>'+
      '<div><div class="wx-temp">'+Math.round(cur.temperature_2m)+u+'</div>'+
      '<div class="wx-rail-cond">'+wx[1]+'</div></div></div>';

    var meta = 'feels '+Math.round(cur.apparent_temperature)+u;
    if (d && d.temperature_2m_max){
      meta += ' · H '+Math.round(d.temperature_2m_max[0])+'° L '+Math.round(d.temperature_2m_min[0])+'°';
    }
    html += '<div class="wx-rail-meta">'+meta+'</div>';

    // hourly: next 6 slots after the current hour (compact)
    var hr = wxData.hourly;
    if (hr && hr.time){
      var now = Date.now(), start = 0;
      for (var i=0;i<hr.time.length;i++){ if (new Date(hr.time[i]).getTime() > now){ start=i; break; } }
      var cells = '';
      for (var j=start;j<start+6 && j<hr.time.length;j++){
        var h = new Date(hr.time[j]).getHours();
        cells += '<div class="h"><span>'+fmtHour(h)+'</span><span class="e">'+wxFor(hr.weather_code[j])[0]+'</span>'+
          '<span class="d">'+Math.round(hr.temperature_2m[j])+'°</span></div>';
      }
      html += '<div class="wx-rail-hours">'+cells+'</div>';
    }

    // next few days
    if (d && d.time){
      var DOW = ['Sun','Mon','Tue','Wed','Thu','Fri','Sat'], drows = '';
      for (var k=1;k<d.time.length && k<=3;k++){
        drows += '<div class="pr"><span>'+DOW[new Date(d.time[k]).getDay()]+' '+wxFor(d.weather_code[k])[0]+'</span>'+
          '<span><b>'+Math.round(d.temperature_2m_max[k])+'°</b> '+Math.round(d.temperature_2m_min[k])+'°</span></div>';
      }
      if (drows) html += '<div class="wx-rail-days">'+drows+'</div>';
    }

    // pollen: show the top 3, with an arrow to reveal the rest (CSS-only via a
    // hidden checkbox — the rows past the third collapse until expanded).
    var pa = pollenSorted(poData);
    if (pa.length){
      var prows = pa.map(function(p){
        return '<div class="pr '+p.lvl.cls+'"><span>'+p.name+'</span><span>'+p.lvl.label+'</span></div>';
      }).join('');
      var more = pa.length > 3;
      html += '<div class="wx-rail-pollen'+(more?' has-more':'')+'">'+
        '<input type="checkbox" id="wxPollenMore" class="wx-pollen-chk">'+ prows +
        (more ? '<label for="wxPollenMore" class="wx-pollen-more">'+
                  '<span class="more">+'+(pa.length-3)+' more &#9662;</span>'+
                  '<span class="less">Show less &#9652;</span></label>' : '')+
        '</div>';
    }
    return html;
  }

  function renderWeather(){
    var maze = document.querySelector('.maze');
    if (!maze) return;
    applyMode();

    var wxData, poData;
    try { wxData = JSON.parse(wxGet('data','null')); } catch(e){}
    try { poData = JSON.parse(wxGet('pollen','null')); } catch(e){}

    var sub  = document.getElementById('wxSubbar');
    var chip = document.getElementById('wxChip');
    var rail = document.getElementById('wxRail');

    if (!wxData || !wxData.current){
      if (sub) sub.innerHTML = ''; if (chip) chip.innerHTML = ''; if (rail) rail.innerHTML = '';
      return;
    }
    var cur = wxData.current, wx = wxFor(cur.weather_code), u = unitSym();
    var temp = Math.round(cur.temperature_2m)+u, feels = Math.round(cur.apparent_temperature)+u;
    var mainHtml = '<span class="wx-icon">'+wx[0]+'</span>'+
      '<span class="wx-temp">'+temp+'</span>'+
      '<span class="wx-cond">'+wx[1]+'</span>';

    // sub-bar: full main + feels + the top 2 allergens
    if (sub){
      sub.innerHTML = '<div class="wx-main">'+mainHtml+'<span class="wx-feels">feels '+feels+'</span></div>'+
        '<div class="pollen-strip">'+pollenPillsTop(poData,2)+'</div>';
    }
    // inline chip: main + a divider + the single strongest allergen
    if (chip){
      var compact = pollenPillsTop(poData,1);
      chip.innerHTML = mainHtml + (compact ? '<span class="wx-div"></span>'+compact : '');
    }
    // rail: hero + hourly + pollen list
    if (rail) rail.innerHTML = railHtml(wxData, poData);
  }

  // Reflect the chosen layout (or "off" when Weather is hidden) onto .maze so
  // the CSS reveals the right container.
  function applyMode(){
    var maze = document.querySelector('.maze');
    if (!maze) return;
    var on = wxGet('show','1') !== '0';
    maze.setAttribute('data-wx-mode', on ? wxGet('mode','inline') : 'off');
  }

  // --- settings controls ---
  function syncWxControls(){
    document.querySelectorAll('[data-wx-toggle]').forEach(function(btn){
      var k = btn.getAttribute('data-wx-toggle');
      var isOn = wxGet(k,'1') !== '0';
      btn.classList.toggle('is-on', isOn);
      btn.setAttribute('aria-pressed', isOn ? 'true' : 'false');
      if (k === 'time24') btn.textContent = isOn ? '24-hour' : 'AM / PM';
    });
    var unit = wxGet('unit','C');
    document.querySelectorAll('[data-wx-unit]').forEach(function(btn){
      var on = btn.getAttribute('data-wx-unit') === unit;
      btn.classList.toggle('is-on', on);
      btn.setAttribute('aria-pressed', on ? 'true' : 'false');
    });
    var mode = wxGet('mode','inline');
    var mi = MODES.indexOf(mode); if (mi < 0) mi = 1;
    var layoutName = document.querySelector('[data-wx-layout-name]');
    if (layoutName) layoutName.textContent = MODE_NAMES[MODES[mi]];
    var layoutDots = document.querySelectorAll('[data-wx-layout-dots] i');
    layoutDots.forEach(function(dot, j){ dot.classList.toggle('is-on', j === mi); });
  }

  document.addEventListener('click', function(e){
    var t = e.target.closest && e.target.closest('[data-wx-toggle],[data-wx-unit]');
    if (!t) return;
    var unit = t.getAttribute('data-wx-unit');
    if (unit){
      if (wxGet('unit','C') !== unit){
        wxSet('unit', unit);
        wxSet('unitSet', '1');    // user chose; stop auto-setting by location
        wxSet('fetchedAt', '0');
        fetchWeather(parseFloat(wxGet('lat','')), parseFloat(wxGet('lon','')));
      }
      syncWxControls();
      return;
    }
    var k = t.getAttribute('data-wx-toggle');
    wxSet(k, wxGet(k,'1') === '0' ? '1' : '0');
    renderWeather();
    syncWxControls();
  });

  var wxLocBtn = document.getElementById('wxLocBtn');
  if (wxLocBtn){
    wxLocBtn.addEventListener('click', function(){
      wxSet('fetchedAt','0');
      requestLocation();
    });
  }

  // Typed place search → Open-Meteo geocoding (free, no key). Picking a result
  // stores its lat/lon/label and, unless the user has set the unit by hand,
  // defaults °F for the US and °C elsewhere.
  var locSearch  = document.getElementById('wxLocSearch');
  var locResults = document.getElementById('wxLocResults');
  if (locSearch && locResults){
    var timer = null;
    function clearResults(){ locResults.innerHTML = ''; locResults.hidden = true; }
    locSearch.addEventListener('input', function(){
      var q = locSearch.value.trim();
      clearTimeout(timer);
      if (q.length < 2){ clearResults(); return; }
      timer = setTimeout(function(){
        fetch('https://geocoding-api.open-meteo.com/v1/search?count=5&language=en&format=json&name='+encodeURIComponent(q))
          .then(function(r){ return r.json(); })
          .then(function(d){
            var list = (d && d.results) || [];
            if (!list.length){ clearResults(); return; }
            locResults.innerHTML = '';
            list.forEach(function(p){
              var place = [p.admin1, p.country].filter(Boolean).join(', ');
              var b = document.createElement('button');
              b.type = 'button'; b.className = 'wx-loc__opt';
              b.innerHTML = '<b>'+p.name+'</b>'+(place ? ' · '+place : '');
              b.addEventListener('click', function(){
                wxSet('lat', p.latitude); wxSet('lon', p.longitude); wxSet('label', p.name);
                if (wxGet('unitSet','0') !== '1') wxSet('unit', p.country_code === 'US' ? 'F' : 'C');
                wxSet('fetchedAt','0');
                locSearch.value = ''; clearResults();
                syncWxControls();
                fetchWeather(p.latitude, p.longitude);
              });
              locResults.appendChild(b);
            });
            locResults.hidden = false;
          })
          .catch(clearResults);
      }, 300);
    });
  }

  var wxLayoutDots = document.querySelector('[data-wx-layout-dots]');
  if (wxLayoutDots) MODES.forEach(function(){ wxLayoutDots.appendChild(document.createElement('i')); });
  function stepLayout(dir){
    var mode = wxGet('mode','inline');
    var i = MODES.indexOf(mode); if (i < 0) i = 1;
    var next = MODES[(i + dir + MODES.length) % MODES.length];
    wxSet('mode', next);
    applyMode(); syncWxControls();
  }
  var wxLayoutPrev = document.querySelector('[data-wx-layout-prev]');
  var wxLayoutNext = document.querySelector('[data-wx-layout-next]');
  if (wxLayoutPrev) wxLayoutPrev.addEventListener('click', function(){ stepLayout(-1); });
  if (wxLayoutNext) wxLayoutNext.addEventListener('click', function(){ stepLayout(1); });

  syncWxControls();
  applyMode();
  if (document.querySelector('.maze')) maybeRefresh();
  document.addEventListener('htmx:afterSwap', function(){
    if (document.querySelector('.maze')) renderWeather();
  });
})();

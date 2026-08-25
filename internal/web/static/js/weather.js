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

  // The browser hands back coordinates, and "My location" is a poor name for a
  // place — the settings field and the rail heading both want a city. This is
  // the one call to a second provider: BigDataCloud's client endpoint is
  // keyless and CORS-open. A failure leaves the generic label and nothing else;
  // no reading of the weather depends on it.
  function reverseGeocode(lat, lon){
    fetch('https://api.bigdatacloud.net/data/reverse-geocode-client?localityLanguage=en&latitude='+lat+'&longitude='+lon)
      .then(function(r){ return r.json(); })
      .then(function(d){
        var name = d && (d.city || d.locality || d.principalSubdivision);
        if (!name) return;
        // Qualified the same way a searched place is, minus the country: this
        // is where you are, so naming your own country adds nothing.
        var region = d.principalSubdivision;
        wxSet('label', name);
        wxSet('place', region && region !== name ? name + ' · ' + region : name);
        syncLocField();
        locNoteSay('');
        renderWeather();
        // Same courtesy the typed search does: match the unit to the country
        // until the user picks one by hand.
        if (wxGet('unitSet','0') !== '1'){
          var unit = d.countryCode === 'US' ? 'F' : 'C';
          if (wxGet('unit','C') !== unit){
            wxSet('unit', unit); wxSet('fetchedAt','0');
            syncWxControls(); fetchWeather(lat, lon);
          }
        }
      })
      .catch(function(){});
  }

  // Records why we have no location ('off' = declined or unsupported) so the
  // placements can say so instead of rendering an unexplained blank. Any
  // location, granted or typed, clears it.
  function requestLocation(){
    if (!navigator.geolocation){ wxSet('geo','off'); renderWeather(); return; }
    navigator.geolocation.getCurrentPosition(function(p){
      wxSet('geo','ok');
      wxSet('lat', p.coords.latitude);
      wxSet('lon', p.coords.longitude);
      wxSet('label', 'My location');
      wxSet('place', 'My location');   // until the reverse lookup names it
      wxSet('fetchedAt', '0');
      locBtnDone();
      syncLocField();
      locNoteSay('Found you. Naming the place…');
      fetchWeather(p.coords.latitude, p.coords.longitude);
      reverseGeocode(p.coords.latitude, p.coords.longitude);
    }, function(){
      wxSet('geo','off');
      locBtnDone();
      locNoteSay('The browser would not share your location — search for a city instead.');
      renderWeather();
    });
  }

  function maybeRefresh(){
    var lat = parseFloat(wxGet('lat',''));
    var lon = parseFloat(wxGet('lon',''));
    if (!lat || !lon){
      // Ask once. A refusal is remembered by the browser, so asking again on
      // every load only re-runs the error path; render the hint instead.
      if (wxGet('geo','') === 'off') renderWeather(); else requestLocation();
      return;
    }
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

    // Nothing to draw yet. If we know a location is never coming, keep the
    // widget's space and say what to do about it; if the request is merely
    // still in flight, stay quiet.
    if (!wxData || !wxData.current){
      var stuck = wxGet('geo','') === 'off' && !wxGet('lat','');
      var hint  = stuck ? '<span class="wx-hint">Weather needs a location. '+
                          'Add one in Settings, or hide the widget.</span>' : '';
      if (sub)  sub.innerHTML  = hint;
      if (chip) chip.innerHTML = stuck ? '<span class="wx-hint">Add a location</span>' : '';
      if (rail) rail.innerHTML = hint;
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
    maze.setAttribute('data-wx-mode', on ? wxGet('mode','subbar') : 'off');
  }

  // --- settings controls ---
  function syncWxControls(){
    var weatherOn = wxGet('show','1') !== '0';
    document.querySelectorAll('[data-wx-toggle]').forEach(function(btn){
      var k = btn.getAttribute('data-wx-toggle');
      var isOn = wxGet(k,'1') !== '0';
      // Pollen rides on Weather: with the widget hidden it has nowhere to
      // render, so it reads and behaves as off while keeping its own pref for
      // when Weather comes back.
      if (k === 'showPollen'){
        btn.disabled = !weatherOn;
        isOn = isOn && weatherOn;
      }
      btn.classList.toggle('is-on', isOn);
      btn.setAttribute('aria-pressed', isOn ? 'true' : 'false');
      if (k === 'time24') btn.textContent = isOn ? '24-hour' : 'AM / PM';
      else if (k === 'show' || k === 'showPollen') btn.textContent = isOn ? 'On' : 'Off';
    });
    var unit = wxGet('unit','C');
    document.querySelectorAll('[data-wx-unit]').forEach(function(btn){
      var on = btn.getAttribute('data-wx-unit') === unit;
      btn.classList.toggle('is-on', on);
      btn.setAttribute('aria-pressed', on ? 'true' : 'false');
    });
    var mode = wxGet('mode','subbar');
    var mi = MODES.indexOf(mode); if (mi < 0) mi = 0;
    var layoutName = document.querySelector('[data-wx-layout-name]');
    if (layoutName) layoutName.textContent = MODE_NAMES[MODES[mi]];
    var layoutDots = document.querySelectorAll('[data-wx-layout-dots] i');
    layoutDots.forEach(function(dot, j){ dot.classList.toggle('is-on', j === mi); });
    syncLocField();
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
    // Switching Weather back on has to resume the load the gate skipped, not
    // just redraw, or the first enable in a session renders nothing.
    if (k === 'show' && wxGet('show','1') !== '0') maybeRefresh();
    else renderWeather();
    syncWxControls();
  });

  // --- Location: a combobox over Open-Meteo's geocoding API (free, no key) ---
  // The field carries the place that is set rather than sitting empty, the list
  // walks with the arrow keys, and every state — searching, no matches, offline
  // — says so. Silence was the old failure: a request that returned nothing
  // looked exactly like one that was never sent.
  var locSearch  = document.getElementById('wxLocSearch');
  var locResults = document.getElementById('wxLocResults');
  var locNote    = document.getElementById('wxLocNote');
  var wxLocBtn   = document.getElementById('wxLocBtn');
  var locHits = [], locActive = -1, locTimer = null, locReq = null, locEditing = false;
  // Set while the last key was a delete, so the field does not re-complete the
  // very text that was just backspaced away.
  var locErasing = false;

  function locBtnDone(){
    if (!wxLocBtn) return;
    wxLocBtn.disabled = false;
    wxLocBtn.textContent = 'Use my location';
  }

  // Two names for one place: 'label' is the bare city, short enough to head the
  // weather rail, and 'place' is the qualified one the settings field shows.
  // "Limerick" is the ambiguous half — the row says which Limerick, and the
  // field has room to keep saying it.
  function locPlaceOf(p){
    var where = [p.admin1, p.country].filter(Boolean).join(', ');
    return where ? p.name + ' · ' + where : p.name;
  }

  // Shows the place that is set. Skipped mid-edit so it never overwrites what
  // is being typed — syncWxControls calls this on every state change.
  function syncLocField(){
    if (!locSearch || locEditing) return;
    locSearch.value = wxGet('place', wxGet('label',''));
  }

  // The city part of whatever is in the field: the qualifier is ours, not
  // something the geocoder will match on, so it never goes into the query.
  function locTyped(){
    return locSearch.value.split(' · ')[0].trim();
  }

  function locNoteSay(msg){
    if (!locNote) return;
    locNote.textContent = msg || '';
    locNote.hidden = !msg;
  }

  function locClose(){
    locHits = []; locActive = -1;
    if (locReq){ locReq.abort(); locReq = null; }
    clearTimeout(locTimer);
    if (!locSearch || !locResults) return;
    locResults.innerHTML = '';
    locResults.hidden = true;
    locSearch.setAttribute('aria-expanded','false');
    locSearch.removeAttribute('aria-activedescendant');
    locNoteSay('');
  }

  function locDraw(){
    if (!locSearch || !locResults) return;
    locResults.innerHTML = '';
    locHits.forEach(function(p, i){
      var where = [p.admin1, p.country].filter(Boolean).join(', ');
      var b = document.createElement('button');
      b.type = 'button';
      b.id = 'wxLocOpt' + i;
      b.className = 'wx-loc__opt' + (i === locActive ? ' is-active' : '') +
        (p.name === wxGet('label','') ? ' is-cur' : '');
      b.setAttribute('role','option');
      b.setAttribute('aria-selected', i === locActive ? 'true' : 'false');
      b.innerHTML = '<span class="wx-loc__nm"><b></b><i></i></span>' +
        '<span class="wx-loc__cc"></span>';
      // Place names are third-party text, so they go in as text, not markup.
      b.querySelector('b').textContent = p.name;
      b.querySelector('i').textContent = where ? ' · ' + where : '';
      b.querySelector('.wx-loc__cc').textContent = p.country_code || '';
      b.addEventListener('click', function(){ locPick(i); });
      locResults.appendChild(b);
    });
    locResults.hidden = !locHits.length;
    locSearch.setAttribute('aria-expanded', locHits.length ? 'true' : 'false');
    if (locActive >= 0){
      locSearch.setAttribute('aria-activedescendant', 'wxLocOpt' + locActive);
      var el = locResults.children[locActive];
      if (el && el.scrollIntoView) el.scrollIntoView({block:'nearest'});
    } else {
      locSearch.removeAttribute('aria-activedescendant');
    }
  }

  // Walking the list carries the name up into the field: the row highlight and
  // the box say the same thing, so Enter is never a guess about which one wins.
  function locMove(step){
    if (!locHits.length) return;
    locActive = (locActive + step + locHits.length) % locHits.length;
    locDraw();
    locSearch.value = locPlaceOf(locHits[locActive]);
  }

  // Inline completion, the way an address bar does it: the rest of the best
  // match is written in and left selected, so typing on replaces it and Enter
  // takes it. Only when the match actually starts with what was typed —
  // otherwise the field would contradict the letters under the cursor.
  function locComplete(typed){
    if (locErasing || locActive < 0) return;
    var place = locPlaceOf(locHits[locActive]);
    if (place.length <= typed.length) return;
    if (place.slice(0, typed.length).toLowerCase() !== typed.toLowerCase()) return;
    locSearch.value = typed + place.slice(typed.length);
    locSearch.setSelectionRange(typed.length, place.length);
  }

  function locPick(i){
    var p = locHits[i];
    if (!p) return;
    wxSet('lat', p.latitude); wxSet('lon', p.longitude);
    wxSet('label', p.name); wxSet('place', locPlaceOf(p));
    wxSet('geo','ok');
    if (wxGet('unitSet','0') !== '1') wxSet('unit', p.country_code === 'US' ? 'F' : 'C');
    wxSet('fetchedAt','0');
    locEditing = false;
    locClose();
    // Written here rather than left to the sync pass: picking a place is the
    // one moment the field must visibly become that place.
    locSearch.value = locPlaceOf(p);
    locSearch.classList.add('is-set');
    setTimeout(function(){ locSearch.classList.remove('is-set'); }, 900);
    syncWxControls();
    fetchWeather(p.latitude, p.longitude);
  }

  // Open-Meteo returns the same town more than once when several records share
  // a name, which reads as a broken list; one row per name-and-region.
  function locDedupe(list){
    var seen = {}, out = [];
    list.forEach(function(p){
      var key = [p.name, p.admin1, p.country_code].join('|').toLowerCase();
      if (seen[key]) return;
      seen[key] = 1; out.push(p);
    });
    return out;
  }

  function locSearchNow(q){
    if (locReq) locReq.abort();
    locReq = new AbortController();
    locNoteSay('Searching…');
    fetch('https://geocoding-api.open-meteo.com/v1/search?count=8&language=en&format=json&name=' +
          encodeURIComponent(q), {signal: locReq.signal})
      .then(function(r){ return r.json(); })
      .then(function(d){
        locReq = null;
        locHits = locDedupe((d && d.results) || []).slice(0, 6);
        locActive = locHits.length ? 0 : -1;
        locNoteSay(locHits.length ? '' : 'No place matches “' + q + '”.');
        locDraw();
        locComplete(q);
      })
      .catch(function(err){
        if (err && err.name === 'AbortError') return;   // superseded, not failed
        locReq = null;
        locHits = []; locActive = -1;
        locDraw();
        locNoteSay('Could not reach the place search.');
      });
  }

  if (locSearch && locResults){
    // Pressing inside the list — an option or its scrollbar — must not blur the
    // field, or the blur handler closes the list out from under the click.
    locResults.addEventListener('mousedown', function(e){ e.preventDefault(); });

    locSearch.addEventListener('input', function(){
      var q = locTyped();
      locEditing = true;
      clearTimeout(locTimer);
      if (locReq){ locReq.abort(); locReq = null; }
      if (q.length < 2){
        locHits = []; locActive = -1; locDraw();
        locNoteSay(q.length ? 'Keep typing…' : '');
        return;
      }
      locTimer = setTimeout(function(){ locSearchNow(q); }, 180);
    });

    // Typing over the current place is the common case, so it comes
    // pre-selected rather than needing to be cleared first.
    locSearch.addEventListener('focus', function(){ locSearch.select(); });

    locSearch.addEventListener('keydown', function(e){
      locErasing = e.key === 'Backspace' || e.key === 'Delete';
      if (e.key === 'ArrowDown'){ e.preventDefault(); locMove(1); return; }
      if (e.key === 'ArrowUp'){ e.preventDefault(); locMove(-1); return; }
      if (e.key === 'Enter'){
        e.preventDefault();
        if (locActive >= 0){ locPick(locActive); return; }
        // Enter before the debounce has fired is impatience, not a mistake.
        var typed = locTyped();
        if (typed.length >= 2){ clearTimeout(locTimer); locSearchNow(typed); }
        return;
      }
      if (e.key === 'Escape'){
        // Only when there is a list to take back: otherwise Escape belongs to
        // the dialog, which is what the user means by it the rest of the time.
        if (locHits.length || locSearch.value !== wxGet('place', wxGet('label',''))){
          e.preventDefault(); e.stopPropagation();
          locEditing = false; locClose(); syncLocField();
        }
      }
    });

    locSearch.addEventListener('blur', function(){
      locEditing = false;
      locClose();
      syncLocField();
    });

    // A click anywhere else is a decision not to pick one.
    document.addEventListener('click', function(e){
      if (locResults.hidden) return;
      if (e.target.closest && e.target.closest('.wx-loc')) return;
      locEditing = false; locClose(); syncLocField();
    });
  }

  if (wxLocBtn){
    wxLocBtn.addEventListener('click', function(){
      // A permission prompt can sit unanswered for a while, and the browser's
      // own dialog does not say which button opened it.
      wxLocBtn.disabled = true;
      wxLocBtn.textContent = 'Locating…';
      locEditing = false; locClose();
      wxSet('fetchedAt','0');
      requestLocation();
    });
  }

  var wxLayoutDots = document.querySelector('[data-wx-layout-dots]');
  if (wxLayoutDots) MODES.forEach(function(){ wxLayoutDots.appendChild(document.createElement('i')); });
  function stepLayout(dir){
    var mode = wxGet('mode','subbar');
    var i = MODES.indexOf(mode); if (i < 0) i = 0;
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
  // Hidden means hidden: no location prompt and no Open-Meteo request.
  if (document.querySelector('.maze') && wxGet('show','1') !== '0') maybeRefresh();
  document.addEventListener('htmx:afterSwap', function(){
    if (document.querySelector('.maze')) renderWeather();
  });
})();

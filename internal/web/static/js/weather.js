// Weather + pollen: fetched from Open-Meteo (current + hourly + daily) and
// its air-quality pollen API, keyed off a location set by geolocation or a
// typed place search. Everything — location, unit, where it shows, show/hide
// — lives in localStorage (no server round-trip, nothing touches the store or
// config). One cached payload (30-min TTL) feeds every live placement.
//
// Two axes. The side menu takes the full read-out and answers up / down / off
// on its own, because it is on every page. The Feed and the Maze share one
// shape — sub-bar, inline, or the full band — and each says whether it shows
// it. Both are reflected onto <html> (data-wx-shape / -feed / -maze / -place)
// where the stylesheet reads them, and reproduced pre-paint by
// static/js/fonts-preload.js so nothing swaps under the reader on load.
//
// A container that isn't on this page, or belongs to a placement that is off,
// is simply left empty — the stylesheet hides an empty one. Nothing here costs
// a page anything it hasn't asked for.
(function(){
  var CACHE_TTL = 30 * 60 * 1000;

  // layout stepper cycle order + display names. This is the SHAPE the widget
  // takes on a page; which pages show it is a separate question, below.
  var MODES = ['subbar','inline','full'];
  var MODE_NAMES = {'subbar':'Sub-bar','inline':'Inline','full':'Full'};

  // Two axes, and they are independent on purpose. The side menu is on every
  // page and carries its own read-out, so it answers "up, down or not at all"
  // by itself. The pages share one shape and each says whether it shows it —
  // one choice for how, two for where, rather than a single list of every
  // combination of the two.
  //
  // Defaults per key, because they are not all '1': the widget has always been
  // the Maze's, and turning it on over the feed you came to read should be
  // something you ask for.
  var DEFAULTS = {show:'1', showPollen:'1', time24:'1', feed:'0', maze:'1'};
  // The switches that mean "show something", and so read as off whenever
  // Weather itself is off. The rest are formats and keep saying what they are.
  var RIDES = {showPollen:1, feed:1, maze:1};
  function wxOn(k){ return wxGet(k, DEFAULTS[k] || '1') !== '0'; }
  function shape(){
    var m = migrate();
    return MODES.indexOf(m) < 0 ? 'subbar' : m;
  }
  // The side menu's positions, as a cycle rather than a row of pills: three
  // states with one of them chosen is the same shape of answer the layout is,
  // and it reads better as the same control. Off leads because it is where the
  // menu starts, and it is what an unreadable stored value falls back to.
  var SIDES = ['off','up','down'];
  var SIDE_NAMES = {'off':'Off','up':'Up','down':'Down'};
  function side(){ var s = wxGet('side','off'); return SIDES.indexOf(s) < 0 ? 'off' : s; }
  function onFeed(){ return wxOn('feed'); }
  function onMaze(){ return wxOn('maze'); }
  // Weather on, and somewhere for it to go. Everything that costs something —
  // the location prompt, the Open-Meteo request — asks this rather than the
  // master switch alone, so "on with every placement off" stays quiet instead
  // of fetching a payload nothing will draw.
  function wanted(){
    return wxOn('show') && (onFeed() || onMaze() || side() !== 'off');
  }

  // The single mode this all replaced, unpicked into the two axes. Rails widened
  // the whole frame to stand beside the board and are gone; a band across the
  // column is what they became. Rewriting the stored keys is the point — left
  // alone, a reader who had chosen one of these would silently fall back to the
  // defaults on their next load.
  var RETIRED = {
    'rail-left'   :{mode:'full',  maze:'1'},
    'rail-right'  :{mode:'full',  maze:'1'},
    'top'         :{mode:'full',  maze:'1'},
    'bottom'      :{mode:'full',  maze:'1'},
    'sidebar-up'  :{mode:'subbar',maze:'0', side:'up'},
    'sidebar-mid' :{mode:'subbar',maze:'0', side:'up'},
    'sidebar-down':{mode:'subbar',maze:'0', side:'down'}
  };
  function migrate(){
    var m = wxGet('mode','subbar'), r = RETIRED[m];
    if (!r) return m;
    wxSet('mode', r.mode);
    wxSet('maze', r.maze);
    wxSet('feed', '0');
    wxSet('side', r.side || 'off');
    return r.mode;
  }

  // Where the current settings would draw on THIS page, or null if nowhere.
  // The side menu is on every page; the rest depends on which page this is,
  // and the containers themselves are the only honest way to tell.
  function placement(){
    if (!wanted()) return null;
    var el = document.getElementById('wxSide');
    if (side() !== 'off' && el) return el;
    var maze = document.querySelector('.maze');
    if (maze) return onMaze() ? maze : null;
    var feed = document.getElementById('wxFeedTile');
    return (feed && onFeed()) ? feed : null;
  }

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
    // Nowhere to draw — but still a redraw, because "nowhere" is the thing that
    // has to be applied: a placement just switched off has a container to
    // empty, and that is what renderWeather does when it finds none.
    if (!placement()){ renderWeather(); return; }
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
  // The full read-out: hero, a feels-like / today H-L line, an hourly strip, a
  // few days, and the complete pollen list. Four groups, and the placement
  // decides whether they stack down the side menu or run along a band — which
  // is why the first three are wrapped: heading, reading and feels-like are one
  // group, and without something to say so the band would lay them out as three.
  // `key` names the container this copy is going into, and it exists for one
  // reason: the pollen expander is a CSS-only checkbox, and a checkbox needs an
  // id for its label to point at. Two placements can be live on one page now —
  // the side menu and a page band — so a fixed id would put two of them in the
  // document and the second label would toggle the first one's list.
  function railHtml(wxData, poData, key){
    var cur = wxData.current, wx = wxFor(cur.weather_code), u = unitSym();
    var d = wxData.daily;
    var html = '<div class="wx-head">'+
      '<div class="wx-rail-lbl">'+(wxGet('label','Weather'))+' · now</div>'+
      '<div class="wx-rail-now"><span class="wx-icon">'+wx[0]+'</span>'+
      '<div><div class="wx-temp">'+Math.round(cur.temperature_2m)+u+'</div>'+
      '<div class="wx-rail-cond">'+wx[1]+'</div></div></div>';

    var meta = 'feels '+Math.round(cur.apparent_temperature)+u;
    if (d && d.temperature_2m_max){
      meta += ' · H '+Math.round(d.temperature_2m_max[0])+'° L '+Math.round(d.temperature_2m_min[0])+'°';
    }
    html += '<div class="wx-rail-meta">'+meta+'</div></div>';

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
        '<input type="checkbox" id="wxPollenMore-'+key+'" class="wx-pollen-chk">'+ prows +
        (more ? '<label for="wxPollenMore-'+key+'" class="wx-pollen-more">'+
                  '<span class="more">+'+(pa.length-3)+' more &#9662;</span>'+
                  '<span class="less">Show less &#9652;</span></label>' : '')+
        '</div>';
    }
    return html;
  }

  function renderWeather(){
    if (!placement()) { applyMode(); clearAll(); return; }
    applyMode();

    var wxData, poData;
    try { wxData = JSON.parse(wxGet('data','null')); } catch(e){}
    try { poData = JSON.parse(wxGet('pollen','null')); } catch(e){}

    // Which container on this page, if any, each shape draws into. A page the
    // reader has switched off returns nothing for all three, and every
    // container that isn't the chosen one is emptied — the CSS hides an empty
    // one, so clearing is what turns a placement off rather than a second rule
    // somewhere saying so.
    var sh = shape(), isFeed = !document.querySelector('.maze');
    var pageOn = isFeed ? onFeed() : onMaze();
    var sub  = pageOn && sh === 'subbar' ? el(isFeed ? 'wxFeedBar'  : 'wxMazeBar')  : null;
    var band = pageOn && sh === 'full'   ? el(isFeed ? 'wxFeedBand' : 'wxMazeBand') : null;
    var tile = pageOn && sh === 'inline' && isFeed ? el('wxFeedTile') : null;
    var chip = pageOn && sh === 'inline' && !isFeed ? el('wxChip') : null;
    var aside = side() !== 'off' ? el('wxSide') : null;
    clearAll([sub, band, tile, chip, aside]);

    // Nothing to draw yet. If we know a location is never coming, keep the
    // widget's space and say what to do about it; if the request is merely
    // still in flight, stay quiet.
    if (!wxData || !wxData.current){
      var stuck = wxGet('geo','') === 'off' && !wxGet('lat','');
      var hint  = stuck ? '<span class="wx-hint">Weather needs a location. '+
                          'Add one in Settings, or hide the widget.</span>' : '';
      var short = stuck ? '<span class="wx-hint">Add a location</span>' : '';
      if (sub)   sub.innerHTML   = hint;
      if (band)  band.innerHTML  = hint;
      if (aside) aside.innerHTML = hint;
      if (chip)  chip.innerHTML  = short;
      if (tile)  tile.innerHTML  = short;
      return;
    }
    var cur = wxData.current, wx = wxFor(cur.weather_code), u = unitSym();
    var temp = Math.round(cur.temperature_2m)+u, feels = Math.round(cur.apparent_temperature)+u;
    var mainHtml = '<span class="wx-icon">'+wx[0]+'</span>'+
      '<span class="wx-temp">'+temp+'</span>'+
      '<span class="wx-cond">'+wx[1]+'</span>';

    // sub-bar: full main + feels + the top 2 allergens
    if (sub){
      sub.innerHTML = opens(
        '<div class="wx-main">'+mainHtml+'<span class="wx-feels">feels '+feels+'</span></div>'+
        '<div class="pollen-strip">'+pollenPillsTop(poData,2)+'</div>',
        wxData, poData, 'bar');
    }
    // inline chip: main + a divider + the single strongest allergen
    if (chip){
      var compact = pollenPillsTop(poData,1);
      chip.innerHTML = opens(mainHtml + (compact ? '<span class="wx-div"></span>'+compact : ''),
        wxData, poData, 'chip');
    }
    // inline on the feed: the reading wearing a stats tile, because that is the
    // furniture the page already has there. One fact and its name, like the
    // three counts beside it — the tile is not the place for the forecast.
    if (tile){
      tile.innerHTML = opens(
        '<div class="num"><span class="wx-icon">'+wx[0]+'</span>'+temp+'</div>'+
        '<div class="label">'+wx[1]+'</div>',
        wxData, poData, 'tile');
    }
    // band or side menu: hero + hourly + days + pollen list
    if (band)  band.innerHTML  = railHtml(wxData, poData, 'band');
    if (aside) aside.innerHTML = railHtml(wxData, poData, 'side');
  }

  // The three compact shapes say one thing each and then stop, which is fine
  // until it isn't — the hour it is about to rain is not in any of them. So the
  // reading opens: it becomes the summary of a <details>, and the panel under it
  // is the same full read-out the side menu draws.
  //
  // A popmenu, deliberately, and not a dialog. It is the app's own disclosure —
  // the feed's filters are all built this way — which means static/js/chrome.js
  // already closes it on a click anywhere else and on Escape, the summary is
  // focusable and toggles on Enter without help, and the panel inherits the
  // glass the other menus are made of. A dialog would have been a new
  // dismissal, a new material and a new keyboard contract for a thing that is
  // one glance deep.
  function opens(summary, wxData, poData, key){
    return '<details class="popmenu wx-pop wx-pop--'+key+'">'+
      '<summary class="wx-pop__btn">'+summary+
        '<span class="wx-pop__mk" aria-hidden="true">&#9662;</span>'+
      '</summary>'+
      '<div class="popmenu__panel wx-pop__panel wx-full">'+railHtml(wxData, poData, key)+'</div>'+
    '</details>';
  }

  function el(id){ return document.getElementById(id); }
  // Empty every container except the ones passed. Two of them can be live at
  // once now — the side menu and a page placement — so this can no longer be
  // "the other one".
  var CONTAINERS = ['wxFeedBar','wxFeedBand','wxFeedTile','wxMazeBar','wxMazeBand','wxChip','wxSide'];
  function clearAll(keep){
    CONTAINERS.forEach(function(id){
      var node = el(id);
      if (node && (!keep || keep.indexOf(node) < 0)) node.innerHTML = '';
    });
  }

  // Reflect the settings onto <html>, where the stylesheet reads them. All of
  // it lives on the root rather than on .maze because two of the three answers
  // — the shape, and the side menu's position — apply to a page that may not
  // have a .maze on it, and the containers themselves are already page-specific
  // enough to keep the rules from reaching where they shouldn't.
  //
  // static/js/fonts-preload.js writes the same three attributes from the same
  // keys before the first paint. That is not a nicety here: the inline shape
  // replaces a tile the server has already rendered, and without it you would
  // watch the placeholder swap for the weather on every load.
  function applyMode(){
    var root = document.documentElement;
    set(root, 'data-wx-shape', wanted() ? shape() : null);
    set(root, 'data-wx-feed',  wanted() && onFeed() ? '1' : null);
    set(root, 'data-wx-maze',  wanted() && onMaze() ? '1' : null);
    set(root, 'data-wx-place', wxOn('show') && side() !== 'off' ? side() : null);
  }
  function set(node, name, value){
    if (value === null) node.removeAttribute(name);
    else node.setAttribute(name, value);
  }

  // --- settings controls ---
  // A switch says its state twice: the knob's side and the word inside it. The
  // word goes into its own span rather than into the button, because the button
  // also holds the knob and writing over its text would throw that away.
  function setSwitch(btn, on, text){
    btn.classList.toggle('is-on', on);
    btn.setAttribute('aria-checked', on ? 'true' : 'false');
    var label = btn.querySelector('.wx-sw__txt');
    if (label) label.textContent = text; else btn.textContent = text;
  }
  // A cycle's name, its position in the strip of dots, and whether it can be
  // stepped at all. Shared by the two of them for the same reason the wiring
  // is: they are one control appearing twice.
  function syncStepper(name, values, labels, value, enabled){
    var i = values.indexOf(value); if (i < 0) i = 0;
    var face = document.querySelector('[data-wx-'+name+'-name]');
    if (face) face.textContent = labels[values[i]];
    var step = document.querySelector('[data-wx-'+name+'-step]');
    if (step) step.classList.toggle('is-off', !enabled);
    document.querySelectorAll('[data-wx-'+name+'-prev],[data-wx-'+name+'-next]').forEach(function(b){
      b.disabled = !enabled;
    });
    document.querySelectorAll('[data-wx-'+name+'-dots] i').forEach(function(dot, j){
      dot.classList.toggle('is-on', j === i);
    });
  }
  function syncWxControls(){
    var weatherOn = wxOn('show');
    document.querySelectorAll('[data-wx-toggle]').forEach(function(btn){
      var k = btn.getAttribute('data-wx-toggle');
      var isOn = wxOn(k);
      // Everything under Weather is unavailable while Weather is off, because
      // none of it has anywhere to take effect. Only the placements read as
      // off with it, though: 24-hour is a format, and a format switch that
      // says AM/PM while 24-hour is what is stored is reporting the wrong
      // preference rather than reporting that it is idle.
      if (k !== 'show') btn.disabled = !weatherOn;
      if (RIDES[k]) isOn = isOn && weatherOn;
      setSwitch(btn, isOn, k === 'time24' ? (isOn ? '24-hour' : 'AM / PM')
                                          : (isOn ? 'On' : 'Off'));
    });
    // Two named states is the shape of a switch too: it wears the unit in force
    // and throws to the other. On rather than off is which side the knob is,
    // not which of them counts as switched on — see .wx-sw--ab.
    document.querySelectorAll('[data-wx-unit]').forEach(function(btn){
      btn.disabled = !weatherOn;
      var f = wxGet('unit','C') === 'F';
      setSwitch(btn, f, f ? '\u00b0F' : '\u00b0C');
    });
    // The shape only has anything to say if a page is showing it; the side
    // menu answers for itself.
    syncStepper('layout', MODES, MODE_NAMES, shape(), weatherOn && (onFeed() || onMaze()));
    syncStepper('side', SIDES, SIDE_NAMES, side(), weatherOn);
    syncLocField();
  }

  // Where the panel opens. Pinned to a corner it opens nowhere near the half of
  // the widget you pressed — the sub-bar runs the width of the column, so its
  // reading and its pollen are a thousand pixels apart and only one of them
  // could ever be right. It opens under the pointer instead, and appears to
  // grow from the point pressed rather than from a corner it has nothing to do
  // with.
  //
  // On pointerdown, so the position is set before <summary> toggles the panel
  // open and it arrives already placed. Clamped to the window because the
  // pointer can be close enough to an edge that a centred panel would hang off
  // it. Both are custom properties rather than inline geometry: the stylesheet
  // still owns where the panel sits, and a keyboard open — which has no pointer
  // to read — falls through to the corner the stylesheet picked.
  document.addEventListener('pointerdown', function(e){
    var btn = e.target.closest && e.target.closest('.wx-pop__btn');
    if (!btn) return;
    var pop = btn.closest('.wx-pop');
    if (!pop) return;
    var edge = 10;
    var w = parseFloat(getComputedStyle(pop).getPropertyValue('--wx-pop-w')) || 260;
    var room = document.documentElement.clientWidth;
    var left = Math.min(Math.max(e.clientX - w / 2, edge), Math.max(edge, room - w - edge));
    pop.style.setProperty('--wx-x', (left - pop.getBoundingClientRect().left) + 'px');
    pop.style.setProperty('--wx-ox', (e.clientX - left) + 'px');
  });

  document.addEventListener('click', function(e){
    var t = e.target.closest && e.target.closest('[data-wx-toggle],[data-wx-unit]');
    if (!t || t.disabled) return;
    if (t.hasAttribute('data-wx-unit')){
      wxSet('unit', wxGet('unit','C') === 'C' ? 'F' : 'C');
      wxSet('unitSet', '1');    // user chose; stop auto-setting by location
      wxSet('fetchedAt', '0');  // the reading itself is in the old unit
      fetchWeather(parseFloat(wxGet('lat','')), parseFloat(wxGet('lon','')));
      syncWxControls();
      return;
    }
    var k = t.getAttribute('data-wx-toggle');
    wxSet(k, wxOn(k) ? '0' : '1');
    // Switching a placement on has to resume the load the gate skipped, not
    // just redraw, or the first enable in a session renders nothing.
    maybeRefresh();
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

  // Both cycles are the same control, so they are wired by the same code and
  // told apart by the name in their attributes. Stepping either one can be the
  // first time this page has had anywhere to draw at all, so it goes through
  // maybeRefresh rather than a redraw: it may have to fetch.
  function wireStepper(name, values, store){
    var dots = document.querySelector('[data-wx-'+name+'-dots]');
    if (dots) values.forEach(function(){ dots.appendChild(document.createElement('i')); });
    function step(dir){
      var i = values.indexOf(store()); if (i < 0) i = 0;
      store(values[(i + dir + values.length) % values.length]);
      applyMode(); syncWxControls(); maybeRefresh();
    }
    var prev = document.querySelector('[data-wx-'+name+'-prev]');
    var next = document.querySelector('[data-wx-'+name+'-next]');
    if (prev) prev.addEventListener('click', function(){ step(-1); });
    if (next) next.addEventListener('click', function(){ step(1); });
  }
  wireStepper('layout', MODES, function(v){ if (v === undefined) return shape(); wxSet('mode', v); });
  wireStepper('side', SIDES, function(v){ if (v === undefined) return side(); wxSet('side', v); });

  syncWxControls();
  applyMode();
  // Hidden, or on with nowhere to show: no location prompt, no request.
  maybeRefresh();
  document.addEventListener('htmx:afterSwap', function(){
    if (placement()) renderWeather();
  });
})();

// The tag-chip input and the tag colour palette, shared by the Maze (tasks) and
// Sources (feeds) pages. Both turn typed text into chips backed by one hidden
// comma-joined field, and both want a tag to be the same colour wherever it
// appears.
//
// This file owns the only global the frontend defines: window.tagput. Everything
// else is delegated document listeners, but the Maze needs to recolour chips
// after its own swaps, so the two need a seam.
(function () {
  // ---- tag colours ----------------------------------------------------
  // Each tag name maps to one of TAG_COLORS palette classes (.tag--cN in the
  // CSS). Default is a stable hash of the name; a click on a row chip cycles it
  // and the choice persists in localStorage, so a tag stays the same colour
  // everywhere it appears.
  var TAG_COLORS = 8;
  function colorStore() { try { return JSON.parse(localStorage.getItem('rh.tagColors') || '{}'); } catch (e) { return {}; } }
  function saveColorStore(m) { try { localStorage.setItem('rh.tagColors', JSON.stringify(m)); } catch (e) {} }
  function tagHash(name) {
    var h = 5381; name = name.toLowerCase();
    for (var i = 0; i < name.length; i++) { h = ((h << 5) + h + name.charCodeAt(i)) >>> 0; }
    return h;
  }
  function colorIndex(name) {
    var m = colorStore(), k = name.toLowerCase();
    return (k in m ? m[k] : tagHash(name)) % TAG_COLORS;
  }
  function colorize(el, name) {
    for (var i = 0; i < TAG_COLORS; i++) { el.classList.remove('tag--c' + i); }
    el.classList.add('tag--c' + colorIndex(name));
  }
  function cycleColor(name) {
    var k = name.toLowerCase(), m = colorStore();
    m[k] = ((k in m ? m[k] : tagHash(name)) + 1) % TAG_COLORS;
    saveColorStore(m);
  }
  // Chips this file renders, plus the read-only ones the server prints.
  function colorChips() {
    document.querySelectorAll('.tagput__chip, .tagput__sug, .src__tag').forEach(function (el) {
      var name = (el.dataset.tag || el.textContent).trim();
      if (name) { colorize(el, name); }
    });
  }

  // ---- tag-chip input -------------------------------------------------
  // Suggestions come from the nearest ancestor carrying data-all-tags, so each
  // page decides what "every tag in use" means without this file knowing.
  function universe(tp) {
    var host = tp.closest('[data-all-tags]');
    var raw = (host && host.dataset.allTags) || '';
    return raw ? raw.split(',') : [];
  }
  function parseList(v) {
    return (v || '').split(',').map(function (s) { return s.trim(); }).filter(Boolean);
  }
  function hidden(tp) { return tp.querySelector('input[type=hidden]'); }
  function tags(tp) { return parseList(hidden(tp).value); }

  function setTags(tp, list) {
    var seen = {}, out = [];
    list.forEach(function (t) {
      t = t.replace(/,/g, '').trim();
      var k = t.toLowerCase();
      if (t && !seen[k]) { seen[k] = 1; out.push(t); }
    });
    hidden(tp).value = out.join(',');
    render(tp);
  }
  function addTags(tp, text) {
    if (text && text.trim()) { setTags(tp, tags(tp).concat(text.split(','))); }
  }
  function render(tp) {
    var chips = tp.querySelector('.tagput__chips');
    chips.innerHTML = '';
    tags(tp).forEach(function (t) {
      var chip = document.createElement('span');
      chip.className = 'tagput__chip';
      chip.dataset.tag = t;
      chip.textContent = t;
      colorize(chip, t);
      var x = document.createElement('button');
      x.type = 'button';
      x.className = 'tagput__x';
      x.setAttribute('aria-label', 'Remove tag ' + t);
      x.innerHTML = '&times;';
      x.addEventListener('click', function () {
        setTags(tp, tags(tp).filter(function (o) { return o !== t; }));
      });
      chip.appendChild(x);
      chips.appendChild(chip);
    });
  }
  function suggest(tp) {
    var input = tp.querySelector('.tagput__input');
    var box = tp.querySelector('.tagput__suggest');
    if (!box) return;
    var q = input.value.trim().toLowerCase();
    var have = {};
    tags(tp).forEach(function (t) { have[t.toLowerCase()] = 1; });
    var matches = universe(tp).filter(function (t) {
      return !have[t.toLowerCase()] && t.toLowerCase().indexOf(q) !== -1;
    });
    box.innerHTML = '';
    if (!matches.length) { box.hidden = true; return; }
    // Anchor the popover under the text input (the last item), not the box's
    // left edge, so it drops from where you're actually typing.
    box.style.left = input.offsetLeft + 'px';
    matches.slice(0, 8).forEach(function (t) {
      var b = document.createElement('button');
      b.type = 'button';
      b.className = 'tagput__sug';
      b.dataset.tag = t;
      b.textContent = t;
      colorize(b, t);
      b.addEventListener('mousedown', function (e) {
        e.preventDefault();
        addTags(tp, t); input.value = ''; suggest(tp); input.focus();
      });
      box.appendChild(b);
    });
    box.hidden = false;
  }
  function init(tp) {
    if (tp.dataset.ready) return;
    tp.dataset.ready = '1';
    var input = tp.querySelector('.tagput__input');
    render(tp);
    input.addEventListener('keydown', function (e) {
      if (e.key === 'Enter' || e.key === ',') {
        e.preventDefault(); addTags(tp, input.value); input.value = ''; suggest(tp);
      } else if (e.key === 'Backspace' && !input.value) {
        var list = tags(tp);
        if (list.length) { setTags(tp, list.slice(0, -1)); }
      }
    });
    input.addEventListener('input', function () { suggest(tp); });
    input.addEventListener('focus', function () { suggest(tp); });
    input.addEventListener('blur', function () {
      addTags(tp, input.value); input.value = '';
      var box = tp.querySelector('.tagput__suggest');
      if (box) { setTimeout(function () { box.hidden = true; }, 120); }
    });
  }
  function initAll() {
    document.querySelectorAll('[data-tagput]').forEach(init);
    colorChips();
  }

  // Commit any pending typed text before an hx-post form submits, so a tag you
  // typed but didn't press Enter on still counts.
  document.addEventListener('submit', function (e) {
    var tp = e.target.querySelector && e.target.querySelector('[data-tagput]');
    if (tp) { var input = tp.querySelector('.tagput__input'); addTags(tp, input.value); input.value = ''; }
  }, true);

  document.addEventListener('DOMContentLoaded', initAll);
  document.body.addEventListener('htmx:afterSwap', initAll);

  window.tagput = { colorize: colorize, cycleColor: cycleColor, colorChips: colorChips, initAll: initAll };
})();

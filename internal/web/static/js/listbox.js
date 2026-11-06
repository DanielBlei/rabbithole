// The themed listbox: a button showing the pick, and a popup of options. The
// choice itself lives in hidden radios inside the menu, so the code that owns
// the preference keeps reading and writing plain radios; picking an option
// checks its radio and fires change (bubbling) for anyone listening.
//
// A menu is any [data-stp-menu] element: Settings writes the markup by hand,
// partials/listbox.html renders the rest. rhListbox.init(root) binds every
// unbound menu inside root, so it is safe to call again after a swap. The
// listeners stay on each menu so Escape is stopped here before the document
// handlers in sources.js and modal.js see it.
//
// Optional attributes: data-default on the menu (the button gets .is-on while
// the pick differs from it) and data-label on an option (the button text when
// the option has no live preview to show).
(function(){
  var openMenu = null;
  var seq = 0;

  function allOpts(m){ return Array.prototype.slice.call(m.querySelectorAll('[role="option"]')); }
  // The options a pointer or key can reach: a hidden one only supplies the
  // button's label while it is picked.
  function opts(m){ return allOpts(m).filter(function(o){ return !o.hidden; }); }
  function radio(m, v){
    return Array.prototype.slice.call(m.querySelectorAll('input[type="radio"]')).filter(function(r){ return r.value === v; })[0];
  }
  function picked(m){
    var r = m.querySelector('input[type="radio"]:checked');
    return r ? r.value : null;
  }
  function sync(m){
    var v = picked(m), cur = m.querySelector('[data-stp-menucur]'), b = m.querySelector('[data-stp-menubtn]'), on = null;
    allOpts(m).forEach(function(o){
      var is = o.dataset.value === v;
      o.setAttribute('aria-selected', is ? 'true' : 'false');
      if (is) on = o;
    });
    if (cur) {
      var p = on && (on.querySelector('.stp-menu__p') || on.querySelector('.stp-menu__n'));
      if (on && on.dataset.label) cur.textContent = on.dataset.label;
      else cur.textContent = p && p.textContent ? p.textContent : (on ? on.dataset.value : '');
    }
    if (b && m.dataset.default !== undefined) b.classList.toggle('is-on', v !== null && v !== m.dataset.default);
  }
  function place(m){
    var b = m.querySelector('[data-stp-menubtn]'), l = m.querySelector('[role="listbox"]');
    var r = b.getBoundingClientRect(), vw = document.documentElement.clientWidth, vh = window.innerHeight;
    // The list is as wide as its longest option and no narrower than the button.
    l.style.setProperty('--lb-w', r.width + 'px');
    var h = l.offsetHeight;
    // It can be wider than the button, so keep it on screen.
    l.style.left = Math.max(8, Math.min(r.left, vw - l.offsetWidth - 8)) + 'px';
    l.style.top = (r.bottom + 6 + h > vh - 8 && r.top - h - 6 > 8 ? r.top - h - 6 : r.bottom + 6) + 'px';
  }
  function active(m, o){
    var l = m.querySelector('[role="listbox"]');
    opts(m).forEach(function(x){ x.classList.toggle('is-active', x === o); });
    if (o) { l.setAttribute('aria-activedescendant', o.id); o.scrollIntoView({block: 'nearest'}); }
  }
  function close(m, back){
    if (!m) return;
    var b = m.querySelector('[data-stp-menubtn]'), l = m.querySelector('[role="listbox"]');
    l.hidden = true;
    b.setAttribute('aria-expanded', 'false');
    if (openMenu === m) openMenu = null;
    if (back) b.focus();
  }
  function open(m){
    if (openMenu && openMenu !== m) close(openMenu);
    var b = m.querySelector('[data-stp-menubtn]'), l = m.querySelector('[role="listbox"]');
    sync(m);
    l.hidden = false;
    b.setAttribute('aria-expanded', 'true');
    place(m);
    openMenu = m;
    l.focus({preventScroll: true});
    active(m, opts(m).filter(function(o){ return o.getAttribute('aria-selected') === 'true'; })[0] || opts(m)[0]);
  }
  function choose(m, o){
    var r = radio(m, o.dataset.value);
    if (r && !r.checked) {
      r.checked = true;
      r.dispatchEvent(new Event('change', {bubbles: true}));
    }
    sync(m);
    close(m, true);
  }

  function bind(m){
    if (m.hasAttribute('data-lb-bound')) return;
    var b = m.querySelector('[data-stp-menubtn]'), l = m.querySelector('[role="listbox"]');
    if (!b || !l) return;
    m.setAttribute('data-lb-bound', '');
    opts(m).forEach(function(o){ if (!o.id) o.id = 'rhlb' + (++seq); });
    b.addEventListener('click', function(){ if (l.hidden) open(m); else close(m); });
    b.addEventListener('keydown', function(e){
      if (e.key === 'ArrowDown' || e.key === 'ArrowUp') { e.preventDefault(); open(m); }
    });
    l.addEventListener('click', function(e){
      var o = e.target.closest('[role="option"]');
      if (o) choose(m, o);
    });
    l.addEventListener('mousemove', function(e){
      var o = e.target.closest('[role="option"]');
      if (o) active(m, o);
    });
    l.addEventListener('keydown', function(e){
      var all = opts(m), i = all.indexOf(all.filter(function(o){ return o.classList.contains('is-active'); })[0]);
      if (e.key === 'ArrowDown') { e.preventDefault(); active(m, all[Math.min(all.length - 1, i + 1)]); }
      else if (e.key === 'ArrowUp') { e.preventDefault(); active(m, all[Math.max(0, i - 1)]); }
      else if (e.key === 'Home') { e.preventDefault(); active(m, all[0]); }
      else if (e.key === 'End') { e.preventDefault(); active(m, all[all.length - 1]); }
      else if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); if (all[i]) choose(m, all[i]); }
      else if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); close(m, true); }
      else if (e.key === 'Tab') close(m);
    });
    m.addEventListener('change', function(){ sync(m); });
    sync(m);
  }

  function init(root){
    root = root || document;
    if (root.nodeType !== 1 && root.nodeType !== 9) return;
    if (root.nodeType === 1 && root.matches('[data-stp-menu]')) bind(root);
    Array.prototype.forEach.call(root.querySelectorAll('[data-stp-menu]'), bind);
  }

  function boundMenus(){ return Array.prototype.slice.call(document.querySelectorAll('[data-lb-bound]')); }

  document.addEventListener('click', function(e){
    if (openMenu && !openMenu.contains(e.target)) close(openMenu);
  });
  window.addEventListener('resize', function(){ close(openMenu); });
  window.addEventListener('scroll', function(e){
    if (openMenu && e.target.nodeType === 1 && openMenu.contains(e.target)) return;
    close(openMenu);
  }, {passive: true, capture: true});
  window.addEventListener('load', function(){ boundMenus().forEach(sync); });
  document.addEventListener('htmx:afterSwap', function(e){ init(e.target); });
  document.addEventListener('htmx:oobAfterSwap', function(e){ init(e.target); });
  document.addEventListener('DOMContentLoaded', function(){ init(document); });

  window.rhListbox = {init: init};
})();

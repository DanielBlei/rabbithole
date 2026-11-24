// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

// Briefing period picker: opens and closes the popover and moves focus around
// its grid. The grid itself is server-rendered and replaced by htmx on month
// changes, so every handler is delegated from the popover or the document.
(function(){
  var btn = document.getElementById('briefingCalBtn');
  var pop = document.getElementById('briefingCal');
  if (!btn || !pop) return;

  function cells(){ return Array.prototype.slice.call(pop.querySelectorAll('.cal__day, .cal__week')); }
  function focusable(){
    return Array.prototype.slice.call(pop.querySelectorAll('button:not([disabled]), a[href]'))
      .filter(function(el){ return el.tabIndex >= 0 || el.matches('button'); });
  }
  function isOpen(){ return !pop.hidden; }

  function open(){
    pop.hidden = false;
    btn.setAttribute('aria-expanded', 'true');
    var target = pop.querySelector('[data-cal-cell][tabindex="0"]') || pop.querySelector('[data-cal-cell]') || pop.querySelector('button');
    if (target) target.focus();
  }
  function close(refocus){
    if (!isOpen()) return;
    pop.hidden = true;
    btn.setAttribute('aria-expanded', 'false');
    if (refocus) btn.focus();
  }

  btn.addEventListener('click', function(){ if (isOpen()) close(true); else open(); });

  document.addEventListener('click', function(e){
    if (isOpen() && !pop.contains(e.target) && !btn.contains(e.target)) close(false);
  });

  pop.addEventListener('focusin', function(e){
    var cur = e.target.closest && e.target.closest('[data-cal-cell]');
    if (!cur) return;
    cells().forEach(function(c){ if (c.hasAttribute('data-cal-cell')) c.tabIndex = -1; });
    cur.tabIndex = 0;
  });

  pop.addEventListener('keydown', function(e){
    if (e.key === 'Escape') {
      e.preventDefault();
      e.stopPropagation();
      close(true);
      return;
    }
    if (e.key === 'Tab') {
      var f = focusable();
      if (!f.length) return;
      var first = f[0], last = f[f.length - 1];
      if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
      else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
      return;
    }
    var cur = e.target.closest && e.target.closest('[data-cal-cell]');
    if (!cur) return;
    var weekly = cur.classList.contains('cal__week');
    var step = { ArrowLeft: -1, ArrowRight: 1, ArrowUp: weekly ? -1 : -7, ArrowDown: weekly ? 1 : 7 }[e.key];
    var all = cells();
    var i = all.indexOf(cur);
    if (step === undefined) {
      if (e.key === 'Home') step = -i;
      else if (e.key === 'End') step = all.length - 1 - i;
      else return;
    }
    e.preventDefault();
    var next = all[i + step];
    if (next && next.hasAttribute('data-cal-cell')) next.focus();
  });
})();

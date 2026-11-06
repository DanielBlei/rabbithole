// The "?" help bubbles (templates/partials/settings_dialogs.html, "stpHint"),
// on every page. CSS opens a bubble on hover or focus; this places it beside
// its button inside the viewport, toggles it on a tap and closes it on Escape,
// a click elsewhere or a scroll. Listeners sit on the document, so bubbles
// swapped in later, dialogs included, work without any binding.
(function(){
  function tipOf(btn){ return btn.nextElementSibling; }
  function hintOf(e){ return e.target.closest && e.target.closest('[data-hint]'); }

  // The right edge is the laid-out page, which stops short of a reserved
  // scrollbar gutter that clientWidth still counts.
  // The bubble is position:fixed. An ancestor with a backdrop filter or
  // transform (a dialog frame) makes that the containing block instead of the
  // viewport, so the offset of the box at 0,0 is measured and subtracted.
  // Inside a dialog frame, which clips, the bubble stays within the frame.
  function place(btn){
    var tip = tipOf(btn), r = btn.getBoundingClientRect(), m = 8;
    var lo = 0, hi = document.documentElement.getBoundingClientRect().right;
    var frame = btn.closest('.modal__frame');
    if (frame) {
      var f = frame.getBoundingClientRect();
      lo = f.left; hi = f.right;
      tip.style.maxWidth = Math.min(320, f.width - 2 * m) + 'px';
    } else {
      tip.style.maxWidth = '';
    }
    var w = tip.offsetWidth, h = tip.offsetHeight;
    var left = Math.max(lo + m, Math.min(r.left, hi - w - m));
    var top = r.bottom + 6;
    if (top + h > window.innerHeight - m && r.top - h - 6 > m) top = r.top - h - 6;
    tip.style.left = '0px';
    tip.style.top = '0px';
    var o = tip.getBoundingClientRect();
    tip.style.left = (left - o.left) + 'px';
    tip.style.top = (top - o.top) + 'px';
  }
  function closeHints(except){
    document.querySelectorAll('[data-hint].is-open').forEach(function(b){
      if (b !== except) b.classList.remove('is-open');
    });
  }

  document.addEventListener('mouseover', function(e){
    var b = hintOf(e);
    if (b) place(b);
  });
  document.addEventListener('focusin', function(e){
    var b = hintOf(e);
    if (b) place(b);
  });
  document.addEventListener('click', function(e){
    var b = hintOf(e);
    closeHints(b);
    if (!b) return;
    place(b);
    b.classList.toggle('is-open');
  });
  document.addEventListener('keydown', function(e){
    if (e.key === 'Escape') {
      var a = document.activeElement;
      var focused = a && a.matches && a.matches('[data-hint]');
      var open = document.querySelector('[data-hint].is-open');
      closeHints();
      if (focused) a.blur();
      // The key dismissed a bubble, so dialogs and editors behind it stay.
      if (focused || open) e.preventDefault();
    }
  });
  window.addEventListener('scroll', function(){ closeHints(); }, {passive: true});
})();

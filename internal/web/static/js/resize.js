// The settings dialog grows from any edge or corner. The size it opens at is
// the smallest it goes, and every opening starts back at that size.
(function(){
  var modal = document.getElementById('settingsModal');
  var frame = modal && modal.querySelector('.modal__frame--stg');
  if (!frame) return;
  var GUTTER = 16;
  var EDGES = ['n','s','e','w','ne','nw','se','sw'];

  EDGES.forEach(function(edge){
    var handle = document.createElement('div');
    handle.className = 'rsz rsz--' + edge;
    handle.setAttribute('aria-hidden', 'true');
    handle.dataset.edge = edge;
    frame.appendChild(handle);
  });

  // The default size, taken when the first drag starts so it matches the
  // theme and viewport the dialog actually opened with.
  var minWidth = 0, minHeight = 0;

  function reset(){
    frame.classList.remove('is-sized');
    ['left','top','width','height'].forEach(function(prop){ frame.style[prop] = ''; });
    minWidth = minHeight = 0;
  }

  new MutationObserver(function(){ if (!modal.hidden) reset(); })
    .observe(modal, {attributes: true, attributeFilter: ['hidden']});

  frame.addEventListener('pointerdown', function(e){
    var handle = e.target.closest('.rsz');
    if (!handle || e.button !== 0) return;
    e.preventDefault();
    var edge = handle.dataset.edge;
    var start = frame.getBoundingClientRect();
    if (!frame.classList.contains('is-sized')){
      minWidth = start.width; minHeight = start.height;
    }
    // Pinned where it is, so each edge moves on its own instead of the
    // centred dialog growing both ways at once.
    frame.classList.add('is-sized');
    place(start.left, start.top, start.width, start.height);
    handle.setPointerCapture(e.pointerId);

    function move(ev){
      var dx = ev.clientX - e.clientX, dy = ev.clientY - e.clientY;
      var left = start.left, top = start.top, right = start.right, bottom = start.bottom;
      var maxRight = window.innerWidth - GUTTER, maxBottom = window.innerHeight - GUTTER;
      if (edge.indexOf('e') >= 0) right  = clamp(start.right + dx,  left + minWidth, maxRight);
      if (edge.indexOf('w') >= 0) left   = clamp(start.left + dx,   GUTTER, right - minWidth);
      if (edge.indexOf('s') >= 0) bottom = clamp(start.bottom + dy, top + minHeight, maxBottom);
      if (edge.indexOf('n') >= 0) top    = clamp(start.top + dy,    GUTTER, bottom - minHeight);
      place(left, top, right - left, bottom - top);
    }
    function end(){
      handle.removeEventListener('pointermove', move);
      handle.removeEventListener('pointerup', end);
      handle.removeEventListener('pointercancel', end);
    }
    handle.addEventListener('pointermove', move);
    handle.addEventListener('pointerup', end);
    handle.addEventListener('pointercancel', end);
  });

  function place(left, top, width, height){
    frame.style.left = left + 'px';
    frame.style.top = top + 'px';
    frame.style.width = width + 'px';
    frame.style.height = height + 'px';
  }
  function clamp(value, low, high){
    return Math.max(low, Math.min(high, value));
  }
})();

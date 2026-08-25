// The modal layers, bottom to top: the settings dialog, then #modal for the
// base htmx dialog, then #modalTop for one stacked over it (the runner and
// settings both open config/feeds there). This owns dismissal, the page scroll
// lock, and keeping focus inside the topmost open frame.
//
// The two htmx layers are containers htmx swaps a fragment into, so they are
// open when they have a child and close by being emptied. Settings is rendered
// with the page and only hides — its controls are bound once at load by
// clock.js/weather.js/fonts.js and must survive. Hence isOpen/close per layer
// rather than one hardcoded test.
(function(){
  function htmxLayer(id){
    var el = document.getElementById(id);
    if (!el) return null;
    return {
      el: el, opener: null, open: false, watch: {childList: true},
      isOpen: function(){ return !!el.firstElementChild; },
      close:  function(){ el.innerHTML = ''; }
    };
  }
  function staticLayer(id){
    var el = document.getElementById(id);
    if (!el) return null;
    return {
      el: el, opener: null, open: false, watch: {attributes: true, attributeFilter: ['hidden']},
      isOpen: function(){ return !el.hidden; },
      close:  function(){ el.hidden = true; }
    };
  }
  var layers = [
    staticLayer('settingsModal'),
    htmxLayer('modal'),
    htmxLayer('modalTop')
  ].filter(Boolean);

  // The layer the user is actually looking at: the highest one that is open.
  function top(){
    for (var i = layers.length - 1; i >= 0; i--){
      if (layers[i].isOpen()) return layers[i];
    }
    return null;
  }
  function frame(){
    var layer = top();
    return layer ? layer.el.querySelector('.modal__frame') : null;
  }

  // Tab stops inside the open frame, in document order.
  function stops(f){
    var sel = 'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';
    return Array.prototype.slice.call(f.querySelectorAll(sel)).filter(function(el){
      return el.getClientRects().length > 0;
    });
  }

  // Mirror the stack's open state onto <html> so the page scroll can lock
  // (html.modal-open in CSS) — covers every open/close path, htmx included.
  // Also where aria-modal gets enforced: focus moves into a layer as it opens
  // and back to whatever opened it as it closes, so closing the stacked dialog
  // lands on the runner link that launched it.
  layers.forEach(function(layer){
    new MutationObserver(function(){
      var open = layer.isOpen();
      document.documentElement.classList.toggle('modal-open', !!top());
      if (open){
        // One modal swapping to another within a layer keeps the original opener.
        if (!layer.open) layer.opener = document.activeElement;
        var f = layer.el.querySelector('.modal__frame');
        if (f){
          // Focused as a container rather than a control, so a screen reader
          // reads the dialog's own label first.
          f.setAttribute('tabindex', '-1');
          f.focus();
        }
      } else if (layer.open){
        // Re-lookup by id: ingest responses OOB-replace their own trigger, so
        // the element captured on open is stale by the time the modal closes.
        var back = layer.opener && layer.opener.id ? document.getElementById(layer.opener.id) : layer.opener;
        if (back && back.isConnected && back.getClientRects().length) back.focus();
        layer.opener = null;
      }
      layer.open = open;
    }).observe(layer.el, layer.watch);
  });

  // Closes one layer at a time, so the runner survives its own child dialog.
  function dismiss(){
    var layer = top();
    if (!layer) return;
    var hadIngest = !!layer.el.querySelector('#ingestBody');
    layer.close();
    // Closing the runner also closes its update channel (the body poll), so
    // ask for a fresh set of OOB chrome fragments — the chip/tab/drawer
    // reflect the run's latest state without a page reload.
    if (hadIngest && window.htmx) htmx.ajax('GET', '/ingest/chrome', {swap: 'none'});
    // Only when the whole stack is gone: the deferred feed refresh must not
    // fire while the user is still reading the runner underneath.
    if (!top()) document.dispatchEvent(new CustomEvent('modal:dismiss'));
  }
  layers.forEach(function(layer){
    layer.el.addEventListener('click', function(e){
      if (e.target.closest('[data-modal-close]')) dismiss();
    });
  });
  document.addEventListener('keydown', function(e){
    if (e.key === 'Escape' && top()) dismiss();
  });

  // Keep Tab inside the open frame so it can't reach the page behind it.
  document.addEventListener('keydown', function(e){
    if (e.key !== 'Tab') return;
    var f = frame();
    if (!f) return;
    var items = stops(f);
    if (!items.length){ e.preventDefault(); f.focus(); return; }
    var first = items[0], last = items[items.length - 1];
    // Focus sits outside after the ingest poll replaces the focused control,
    // and on the frame itself right after open — both re-enter at an edge.
    if (document.activeElement === f || !f.contains(document.activeElement)){
      e.preventDefault();
      (e.shiftKey ? last : first).focus();
    } else if (e.shiftKey && document.activeElement === first){
      e.preventDefault(); last.focus();
    } else if (!e.shiftKey && document.activeElement === last){
      e.preventDefault(); first.focus();
    }
  });
})();
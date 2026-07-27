// The #modal container: htmx swaps a fragment in, this owns dismissal, the page
// scroll lock, and keeping focus inside the open frame.
(function(){
  var modal = document.getElementById('modal');
  var opener = null;
  var wasOpen = false;

  function frame(){ return modal.querySelector('.modal__frame'); }

  // Tab stops inside the open frame, in document order.
  function stops(f){
    var sel = 'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';
    return Array.prototype.slice.call(f.querySelectorAll(sel)).filter(function(el){
      return el.getClientRects().length > 0;
    });
  }

  // Mirror the modal's open state onto <html> so the page scroll can lock
  // (html.modal-open in CSS) — covers every open/close path, htmx included.
  // Also where aria-modal gets enforced: focus moves into the frame on open
  // and back to whatever opened it on close.
  new MutationObserver(function(){
    var f = frame();
    document.documentElement.classList.toggle('modal-open', !!modal.firstElementChild);
    if (f){
      // One modal swapping to another keeps the original opener.
      if (!wasOpen) opener = document.activeElement;
      // Focused as a container rather than a control, so a screen reader
      // reads the dialog's own label first.
      f.setAttribute('tabindex', '-1');
      f.focus();
    } else if (wasOpen){
      // Re-lookup by id: ingest responses OOB-replace their own trigger, so
      // the element captured on open is stale by the time the modal closes.
      var back = opener && opener.id ? document.getElementById(opener.id) : opener;
      if (back && back.isConnected && back.getClientRects().length) back.focus();
      opener = null;
    }
    wasOpen = !!f;
  }).observe(modal, {childList: true});
  function dismiss(){
    var hadIngest = !!document.getElementById('ingestBody');
    modal.innerHTML = '';
    // Closing the runner also closes its update channel (the body poll), so
    // ask for a fresh set of OOB chrome fragments — the chip/tab/drawer
    // reflect the run's latest state without a page reload.
    if (hadIngest && window.htmx) htmx.ajax('GET', '/ingest/chrome', {swap: 'none'});
    document.dispatchEvent(new CustomEvent('modal:dismiss'));
  }
  modal.addEventListener('click', function(e){
    if (e.target.closest('[data-modal-close]')) dismiss();
  });
  document.addEventListener('keydown', function(e){
    if (e.key === 'Escape' && modal.firstElementChild) dismiss();
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

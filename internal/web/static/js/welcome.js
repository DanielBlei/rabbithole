// First-run welcome. The server renders it only while it is needed and keeps
// which steps were taken; this opens it and reports each step as it happens.
(function(){
  var dialog = document.getElementById('welcomeModal');
  if (!dialog) return;

  // Resolves true only on the 204 the server answers a recorded step with —
  // fetch rejects on network failure but not on a 500, so the status is read
  // rather than trusted. A step that fails to record is asked about again next
  // load; the dismissal below refuses to close on it instead.
  function record(step){
    return fetch('/welcome/' + step, {method: 'POST'}).then(function(r){
      return r.status === 204;
    }).catch(function(){
      return false;
    });
  }
  dialog.hidden = false;

  dialog.addEventListener('click', function(e){
    var button = e.target.closest('[data-welcome-step]');
    if (!button || button.classList.contains('is-done')) return;
    button.classList.add('is-done');
    record(button.dataset.welcomeStep);
  });
  // The ×, the backdrop, Escape and "got it" all close through modal.js. The
  // dismissal is held until the server confirms it, so a failed write cannot
  // close the welcome and bring it back on the next page.
  document.addEventListener('modal:beforeclose', function(e){
    if (e.detail.layer !== dialog) return;
    e.preventDefault();
    record('dismissed').then(function(ok){
      if (ok) e.detail.proceed();
    });
  });
})();

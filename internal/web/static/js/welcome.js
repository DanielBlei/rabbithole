// First-run welcome. The server renders it only while it is needed and keeps
// which steps were taken; this opens it and reports each step as it happens.
(function(){
  var dialog = document.getElementById('welcomeModal');
  if (!dialog) return;

  function record(step){
    fetch('/welcome/' + step, {method: 'POST'}).catch(function(){ /* asked again next load */ });
  }
  dialog.hidden = false;

  dialog.addEventListener('click', function(e){
    var button = e.target.closest('[data-welcome-step]');
    if (!button || button.classList.contains('is-done')) return;
    button.classList.add('is-done');
    record(button.dataset.welcomeStep);
  });
  // The ×, the backdrop, Escape and "got it" all close through modal.js.
  document.addEventListener('modal:beforeclose', function(e){
    if (e.detail.layer === dialog) record('dismissed');
  });
})();

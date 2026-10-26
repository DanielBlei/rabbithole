// Unsaved changes in the profile editor. Leaving the page or any other editor
// action first asks to save, discard or keep editing. Switching settings tabs
// does not: the editor stays in its pane, edits and all.
(function(){
  var askLayer = document.getElementById('modalTop');
  if (!document.getElementById('profilesSettings') || !askLayer) return;
  var snapshot = null, snapshotForm = null;

  function editorForm(){
    return document.querySelector('#profilesSettings .prof-form form[hx-post]');
  }
  function fieldValues(form){
    var values = [];
    new FormData(form).forEach(function(value, name){ values.push([name, value]); });
    return JSON.stringify(values);
  }
  function takeSnapshot(){
    snapshotForm = editorForm();
    snapshot = snapshotForm ? fieldValues(snapshotForm) : null;
  }
  function dirty(){
    var form = editorForm();
    return !!form && snapshot !== null && fieldValues(form) !== snapshot;
  }

  takeSnapshot();
  // Retaken only when a new editor arrives. A rejected save comes back with
  // the edits still in it, so it keeps the old snapshot and stays unsaved.
  document.addEventListener('htmx:afterSwap', function(){
    var form = editorForm();
    if (form === snapshotForm) return;
    if (form && form.closest('.prof-form').querySelector('.prof-form__err')){ snapshotForm = form; return; }
    takeSnapshot();
  });

  // Holds an action until the user picks. Save continues only once the save
  // went through.
  function ask(proceed){
    askLayer.innerHTML =
      '<div class="modal">' +
        '<div class="modal__backdrop" data-modal-close></div>' +
        '<div class="modal__frame modal__frame--ask" role="dialog" aria-modal="true" aria-labelledby="profileUnsavedTitle">' +
          '<div class="modal__bar">' +
            '<span class="modal__title" id="profileUnsavedTitle">Unsaved changes</span>' +
            '<button class="modal__close" type="button" data-modal-close aria-label="Close">&times;</button>' +
          '</div>' +
          '<div class="prof-confirm">' +
            '<p>This profile has changes that are not saved yet.</p>' +
            '<p class="prof-form__err" role="alert" hidden></p>' +
          '</div>' +
          '<div class="modal__foot prof-confirm__actions">' +
            '<button class="clk-btn" type="button" data-modal-close>Keep editing</button>' +
            '<button class="clk-btn clk-btn--danger" type="button" data-unsaved="discard">Discard</button>' +
            '<button class="clk-btn clk-btn--go" type="button" data-unsaved="save">Save</button>' +
          '</div>' +
        '</div>' +
      '</div>';
    var error = askLayer.querySelector('.prof-form__err');
    askLayer.querySelector('[data-unsaved="discard"]').addEventListener('click', function(){
      askLayer.innerHTML = '';
      proceed();
    });
    askLayer.querySelector('[data-unsaved="save"]').addEventListener('click', function(){
      var form = editorForm();
      if (!form) { askLayer.innerHTML = ''; proceed(); return; }
      // Browser validation points at the field once this dialog has closed
      // and modal.js has handed focus back.
      if (!form.checkValidity()){
        askLayer.innerHTML = '';
        setTimeout(function(){ form.reportValidity(); }, 0);
        return;
      }
      form.addEventListener('htmx:afterRequest', function done(e){
        form.removeEventListener('htmx:afterRequest', done);
        if (!e.detail.successful){
          error.textContent = 'Could not save the profile. Try again, or keep editing.';
          error.hidden = false;
          return;
        }
        askLayer.innerHTML = '';
        if (!document.querySelector('#profilesSettings .prof-form__err')) proceed();
      });
      form.requestSubmit();
    });
  }

  // Following a link away from the page. The settings tab bar is not one of
  // these: its links only switch panes.
  document.addEventListener('click', function(e){
    var link = e.target.closest('a[href]');
    if (!link || link.closest('[data-stp-tab]') || link.hasAttribute('data-stp-tab')) return;
    if (e.defaultPrevented || e.button !== 0 || e.ctrlKey || e.metaKey || e.shiftKey || e.altKey) return;
    if (link.target && link.target !== '_self') return;
    if (link.origin !== window.location.origin || link.hasAttribute('hx-get') || !dirty()) return;
    e.preventDefault();
    ask(function(){ snapshot = null; window.location.href = link.href; });
  });

  // Reload, back out of the page or close the tab: the browser's own prompt.
  window.addEventListener('beforeunload', function(e){
    if (!dirty()) return;
    e.preventDefault();
    e.returnValue = '';
  });

  // Any other action inside the editor. The request is replayed rather than
  // resumed, because a save swaps out the element that issued it.
  document.addEventListener('htmx:confirm', function(e){
    var panel = document.getElementById('profilesSettings');
    var elt = e.detail.elt;
    if (!panel || !panel.contains(elt) || elt === editorForm() || !dirty()) return;
    e.preventDefault();
    var verb = e.detail.verb.toUpperCase(), path = e.detail.path;
    var target = e.detail.target && e.detail.target.id ? '#' + e.detail.target.id : e.detail.target;
    var swap = elt.getAttribute('hx-swap') || 'innerHTML';
    ask(function(){ htmx.ajax(verb, path, {target: target, swap: swap}); });
  });
})();

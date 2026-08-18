/* Confirmation dialogs.

   Replaces window.confirm, which cannot be styled, blocks the whole tab, and
   reads as a browser error rather than part of the app. Built on <dialog> so
   focus trapping, the backdrop and Escape come from the platform.

   Returns a promise, so callers decide asynchronously:

     DL.confirm({ title: '…', body: '…', confirm: 'Discard' })
       .then(ok => { if (ok) … });
*/

(function () {
  'use strict';

  var esc = window.DL.escapeHTML;
  var el = null;

  function build() {
    if (el) return el;
    el = document.createElement('dialog');
    el.className = 'dl-dialog';
    el.innerHTML =
      '<form method="dialog" class="dl-dialog-form">' +
      '<h2 class="dl-dialog-title"></h2>' +
      '<p class="dl-dialog-body"></p>' +
      '<div class="dl-dialog-actions">' +
      '<button value="cancel" class="btn dl-dialog-cancel" type="submit"></button>' +
      '<button value="confirm" class="btn btn-primary dl-dialog-confirm" type="submit"></button>' +
      '</div>' +
      '</form>';
    document.body.appendChild(el);
    return el;
  }

  // confirm resolves true when the user accepts. Cancelling — including via
  // Escape or the backdrop — resolves false, so the safe answer is the default.
  function confirmDialog(opts) {
    opts = opts || {};
    var d = build();

    d.querySelector('.dl-dialog-title').textContent = opts.title || 'Are you sure?';
    var body = d.querySelector('.dl-dialog-body');
    // bodyHTML is for callers that need structure -- a list of imported files,
    // say. Everything in it is the caller's responsibility to escape.
    if (opts.bodyHTML) {
      body.innerHTML = opts.bodyHTML;
    } else {
      body.textContent = opts.body || '';
    }
    body.hidden = !(opts.body || opts.bodyHTML);

    var confirmBtn = d.querySelector('.dl-dialog-confirm');
    var cancelBtn = d.querySelector('.dl-dialog-cancel');
    confirmBtn.textContent = opts.confirm || 'Confirm';
    // An empty cancel label means there is nothing to cancel: the dialog is
    // telling you something rather than asking.
    cancelBtn.textContent = opts.cancel || 'Cancel';
    cancelBtn.hidden = opts.cancel === '';
    confirmBtn.classList.toggle('btn-danger', !!opts.danger);
    confirmBtn.classList.toggle('btn-primary', !opts.danger);

    return new Promise(function (resolve) {
      function done() {
        d.removeEventListener('close', done);
        resolve(d.returnValue === 'confirm');
      }
      d.addEventListener('close', done);
      d.returnValue = 'cancel';
      d.showModal();
      // Focus the safe option, so a stray Enter does not confirm a destructive
      // action.
      (opts.danger ? cancelBtn : confirmBtn).focus();
    });
  }

  // anyOpen lets other Escape handlers stand down while a dialog is up.
  function anyOpen() {
    return !!document.querySelector('dialog.dl-dialog[open]');
  }

  window.DL = window.DL || {};
  window.DL.confirm = confirmDialog;
  window.DL.dialogOpen = anyOpen;
})();

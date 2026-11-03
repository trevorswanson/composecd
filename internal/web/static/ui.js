// Actions report on the button that started them: it spins while the action
// runs, then shows a check or a cross for a moment. Live updates re-render the
// page under a running action, so its state is kept by the form's action (and
// the row it names) and painted again onto whichever button is there now.
var actions = {};
function actionKey(form) {
  var key = form.getAttribute('hx-post');
  form.querySelectorAll('input[type=hidden]:not([name=csrf])').forEach(function (input) {
    key += '|' + input.name + '=' + input.value;
  });
  return key;
}
function actionForm(elt) {
  var form = elt && elt.closest && elt.closest('form[hx-post]');
  return form && form.querySelector('button[type=submit]') ? form : null;
}
function paint(form) {
  var button = form.querySelector('button[type=submit]');
  var state = actions[actionKey(form)];
  if (!button || button.dataset.painted === state) { return; }
  button.classList.remove('is-pending', 'is-done', 'is-failed');
  if (button.dataset.painted === 'pending') {
    button.disabled = button.dataset.wasDisabled === 'true';
    button.removeAttribute('aria-busy');
  }
  if (state) { button.classList.add('is-' + state); }
  if (state === 'pending') {
    button.dataset.wasDisabled = button.disabled;
    button.disabled = true;
    button.setAttribute('aria-busy', 'true');
  }
  if (state) { button.dataset.painted = state; } else { delete button.dataset.painted; }
}
function paintAll() {
  document.querySelectorAll('form[hx-post]').forEach(paint);
}
function settle(key, state) {
  actions[key] = state;
  paintAll();
  setTimeout(function () {
    if (actions[key] === state) { delete actions[key]; paintAll(); }
  }, state === 'failed' ? 4000 : 2000);
}
document.addEventListener('htmx:beforeRequest', function (event) {
  var form = actionForm(event.detail.elt);
  if (!form) { return; }
  var key = actionKey(form);
  actions[key] = 'pending';
  paint(form);
  // The request's end, not htmx's events: a response that re-renders the form
  // detaches it first, and events on it no longer reach the document.
  event.detail.xhr.addEventListener('loadend', function () {
    var xhr = event.detail.xhr;
    var ok = xhr.status >= 200 && xhr.status < 400;
    settle(key, ok ? 'done' : 'failed');
    // A success makes an earlier failure stale.
    if (ok) {
      document.querySelectorAll('#action-result > .callout.bad').forEach(function (box) { box.remove(); });
    }
  });
});
document.addEventListener('htmx:afterSettle', paintAll);
document.addEventListener('htmx:oobAfterSwap', paintAll);
// Surface failures in the result area, where they stay until dismissed.
function failure(title, text) {
  var result = document.getElementById('action-result');
  if (!result) { return; }
  var box = document.createElement('div');
  box.className = 'callout bad';
  box.setAttribute('role', 'alert');
  var body = document.createElement('div');
  var strong = document.createElement('strong');
  strong.textContent = title;
  body.append(strong);
  if (text) {
    var detail = document.createElement('p');
    detail.className = 'detail';
    detail.textContent = text;
    body.append(detail);
  }
  var close = document.createElement('button');
  close.type = 'button';
  close.className = 'btn btn-ghost btn-sm btn-icon i-close callout-close';
  close.setAttribute('aria-label', 'Dismiss');
  close.dataset.dismiss = '';
  box.append(body, close);
  result.replaceChildren(box);
}
document.addEventListener('htmx:responseError', function (event) {
  var form = actionForm(event.detail.elt);
  var button = form && form.querySelector('button[type=submit]');
  var what = button && (button.textContent.trim() || button.title || button.getAttribute('aria-label'));
  failure(what ? what + ' failed' : 'That didn’t work', event.detail.xhr.responseText.trim());
});
document.addEventListener('htmx:sendError', function () {
  failure('Couldn’t reach composecd', 'Check your connection, then try again.');
});
document.addEventListener('click', function (event) {
  var close = event.target.closest('[data-dismiss]');
  if (close) { close.closest('.callout').remove(); return; }
  var toast = event.target.closest('.toast');
  if (toast) { toast.remove(); }
});
document.addEventListener('animationend', function (event) {
  if (event.animationName === 'mf-toast-out') { event.target.remove(); }
});
// An operation's result says how it ended once its output does.
document.addEventListener('htmx:sseBeforeMessage', function (event) {
  var box = event.target.closest('.callout[data-op]');
  if (box && event.detail.type === 'error' && event.detail.data) { box.dataset.failed = 'true'; }
});
document.addEventListener('htmx:sseClose', function (event) {
  var box = event.target.closest && event.target.closest('.callout[data-op]');
  if (!box || event.detail.type !== 'message') { return; }
  var failed = box.dataset.failed === 'true';
  box.classList.remove('busy');
  box.classList.add(failed ? 'bad' : 'ok');
  box.querySelector('strong').textContent = box.dataset.op + (failed ? ' failed' : ' succeeded');
  box.querySelectorAll('.log').forEach(function (log) { log.dataset.ended = ''; });
});
// Show in the sidebar's root state when the page's live updates disconnect.
// Updates only arrive while connected, and each re-renders the root state, so
// a reconnect restores it.
document.addEventListener('htmx:sseOpen', function (event) {
  var root = document.getElementById('root-status');
  if (event.target.id === 'events' && root) { root.removeAttribute('data-conn'); }
});
document.addEventListener('htmx:sseError', function (event) {
  var root = document.getElementById('root-status');
  if (event.target.id !== 'events' || !root) { return; }
  root.dataset.conn = 'lost';
  var what = root.querySelector('.what');
  if (what) { what.textContent = 'Reconnecting'; }
});
// Keep a long-running tail bounded while allowing someone to scroll back.
document.addEventListener('htmx:sseMessage', function (event) {
  var log = event.target;
  if (!log.classList || !log.classList.contains('log')) { return; }
  var follow = log.scrollHeight - log.scrollTop - log.clientHeight < 80;
  while (log.children.length > 2000) { log.firstElementChild.remove(); }
  if (follow) { log.scrollTop = log.scrollHeight; }
});
// Open and close the main menu on narrow screens, where it folds behind a button.
document.addEventListener('click', function (event) {
  var toggle = event.target.closest('.nav-toggle');
  if (!toggle) { return; }
  var open = toggle.getAttribute('aria-expanded') !== 'true';
  toggle.setAttribute('aria-expanded', String(open));
  toggle.setAttribute('aria-label', open ? 'Close menu' : 'Menu');
});
// Links and cards with data-dialog open their URL's fragment in the page's
// dialog; without script they are ordinary links to the same fragment.
document.addEventListener('click', function (event) {
  var opener = event.target.closest('[data-dialog]');
  var dialog = document.getElementById('dialog');
  if (!opener || !dialog || event.defaultPrevented || event.metaKey || event.ctrlKey) { return; }
  event.preventDefault();
  htmx.ajax('GET', opener.dataset.dialog, { target: '#dialog-body', swap: 'innerHTML' }).then(function () {
    if (!dialog.open) { dialog.showModal(); }
  });
});
document.addEventListener('click', function (event) {
  var dialog = document.getElementById('dialog');
  if (!dialog || !dialog.open) { return; }
  // The backdrop is the dialog element itself; its content sits in children.
  if (event.target.closest('[data-close]') || event.target === dialog) { dialog.close(); }
});
// Closing the dialog drops its content, which ends any output it was following.
document.addEventListener('close', function (event) {
  if (event.target.id === 'dialog') { document.getElementById('dialog-body').replaceChildren(); }
}, true);
// Forms marked data-autosubmit apply as soon as one of their inputs changes.
document.addEventListener('change', function (event) {
  var form = event.target.closest('form[data-autosubmit]');
  if (form) { form.requestSubmit(); }
});

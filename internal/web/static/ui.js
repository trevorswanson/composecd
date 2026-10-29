// Surface HTTP failures in the same accessible result area as successful actions.
document.addEventListener('htmx:responseError', function (event) {
  var result = document.getElementById('action-result');
  if (!result) { return; }
  var box = document.createElement('div');
  box.className = 'callout bad';
  box.setAttribute('role', 'alert');
  var body = document.createElement('div');
  var title = document.createElement('strong');
  title.textContent = 'That didn’t work';
  var detail = document.createElement('p');
  detail.textContent = event.detail.xhr.responseText;
  body.append(title, detail);
  box.append(body);
  result.replaceChildren(box);
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

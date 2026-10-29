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
// Show whether the page's live updates are connected.
function setLive(state, text) {
  var live = document.getElementById('live');
  if (live) { live.dataset.state = state; live.textContent = text; }
}
document.addEventListener('htmx:sseOpen', function (event) {
  if (event.target.id === 'events') { setLive('open', 'Live'); }
});
document.addEventListener('htmx:sseError', function (event) {
  if (event.target.id === 'events') { setLive('closed', 'Reconnecting'); }
});
// Keep a long-running tail bounded while allowing someone to scroll back.
document.addEventListener('htmx:sseMessage', function (event) {
  var log = event.target;
  if (!log.classList || !log.classList.contains('log')) { return; }
  var follow = log.scrollHeight - log.scrollTop - log.clientHeight < 80;
  while (log.children.length > 2000) { log.firstElementChild.remove(); }
  if (follow) { log.scrollTop = log.scrollHeight; }
});

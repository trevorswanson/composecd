// Surface HTTP failures in the same accessible result area as successful actions.
document.addEventListener('htmx:responseError', function (event) {
  var result = document.getElementById('action-result');
  if (result) { result.textContent = event.detail.xhr.responseText; }
});
// Keep a long-running tail bounded while allowing someone to scroll back.
document.addEventListener('htmx:sseMessage', function (event) {
  var log = event.target;
  if (!log.classList || !log.classList.contains('log')) { return; }
  var follow = log.scrollHeight - log.scrollTop - log.clientHeight < 80;
  while (log.children.length > 2000) { log.firstElementChild.remove(); }
  if (follow) { log.scrollTop = log.scrollHeight; }
});

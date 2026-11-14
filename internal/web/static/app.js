/* PostgreSQL notifications trigger HTMX fragment refreshes; periodic refresh recovers missed signals. */
htmx.config.allowEval = false;
htmx.config.allowScriptTags = false;
const stream = new EventSource('/events');
const status = document.getElementById('connection');
let pending;
stream.onopen = () => { status.textContent = 'Live'; status.dataset.state = 'live'; };
stream.onerror = () => { status.textContent = 'Reconnecting'; status.dataset.state = 'offline'; };
stream.addEventListener('change', () => {
  clearTimeout(pending);
  pending = setTimeout(() => htmx.trigger(document.body, 'workflow-change'), 150);
});
// Keep the selected repository in the URL without navigating away from live updates.
document.body.addEventListener('change', event => {
  if (event.target.id === 'repository') {
    const url = new URL(location.href);
    if (event.target.value) url.searchParams.set('repo', event.target.value);
    else url.searchParams.delete('repo');
    history.replaceState(null, '', url);
  }
});
// Preserve expanded run details across automatic refreshes.
let expanded = [];
document.body.addEventListener('htmx:beforeSwap', () => {
  expanded = [...document.querySelectorAll('details[open][id]')].map(el => el.id);
});
document.body.addEventListener('htmx:afterSwap', () => {
  expanded.forEach(id => { const el = document.getElementById(id); if (el) el.open = true; });
});

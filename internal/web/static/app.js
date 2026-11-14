htmx.config.allowEval = false;
htmx.config.allowScriptTags = false;
const repository = document.getElementById('repository');
const connection = document.getElementById('connection');
const modal = document.getElementById('run-modal');
const modalContent = document.getElementById('modal-content');
let source;
let selectedRun;
let knownRepositories = [...document.querySelectorAll('[data-repo]')].map(button => button.dataset.repo).filter(Boolean);
let lastHTML;

function formatDuration(milliseconds) {
  const seconds = Math.max(0, Math.floor(milliseconds / 1000));
  const hours = Math.floor(seconds / 3600);
  const minutes = Math.floor(seconds % 3600 / 60);
  if (hours >= 24) return `${Math.floor(hours / 24)}d ${hours % 24}h`;
  if (hours) return `${hours}h ${minutes}m ${seconds % 60}s`;
  if (minutes) return `${minutes}m ${seconds % 60}s`;
  return `${seconds}s`;
}
function tick() {
  document.querySelectorAll('time[data-clock]').forEach(clock => {
    const timestamp = Date.parse(clock.dateTime);
    if (!Number.isFinite(timestamp)) return;
    clock.textContent = formatDuration(Date.now() - timestamp) + (clock.dataset.clock === 'ago' ? ' ago' : '');
  });
}
function setStatus(text, state) {
  connection.setAttribute('aria-label', text);
  document.getElementById('connection-tip').textContent = text;
  connection.dataset.state = state;
}
const picker = document.getElementById('repo-picker');
const search = document.getElementById('repo-search');
const options = document.getElementById('repo-options');
function updateOptions() {
  const query = search.value.trim().toLowerCase();
  const repos = [...new Set(['', ...knownRepositories, ...(repository.value ? [repository.value] : [])])];
  options.replaceChildren(...repos.filter(repo => !repo || repo.toLowerCase().includes(query)).map(repo => {
    const button = document.createElement('button');
    button.type = 'button';
    button.dataset.repo = repo;
    button.textContent = repo || 'All repositories';
    button.setAttribute('aria-pressed', String(repo === repository.value));
    return button;
  }));
  document.getElementById('repo-empty').hidden = !query || repos.some(repo => repo && repo.toLowerCase().includes(query));
}
search.addEventListener('input', updateOptions);
picker.addEventListener('toggle', () => {
  if (picker.open) { search.value = ''; updateOptions(); search.focus(); }
});
options.addEventListener('click', event => {
  const button = event.target.closest('[data-repo]');
  if (!button) return;
  repository.value = button.dataset.repo;
  document.getElementById('repo-picker-label').firstChild.textContent = repository.value || 'All repositories';
  picker.open = false;
  picker.querySelector('summary').focus();
  repository.dispatchEvent(new Event('change'));
});
document.addEventListener('click', event => { if (!picker.contains(event.target)) picker.open = false; });
picker.addEventListener('keydown', event => {
  if (event.key === 'Escape') { picker.open = false; picker.querySelector('summary').focus(); }
  if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
    const buttons = [...options.querySelectorAll('button')];
    const index = buttons.indexOf(document.activeElement);
    const next = event.key === 'ArrowDown' ? (index + 1) % buttons.length : (index <= 0 ? buttons.length - 1 : index - 1);
    buttons[next]?.focus();
    event.preventDefault();
  }
});
function refreshModal() {
  if (!modal.open || !selectedRun) return;
  const template = document.getElementById(selectedRun);
  if (!template) return; // Keep open details if the run ages out of its column.
  const previousScroll = modalContent.scrollTop;
  modalContent.replaceChildren(template.content.cloneNode(true));
  modalContent.scrollTop = previousScroll;
}
function connect() {
  if (source) source.close();
  setStatus('Connecting', 'connecting');
  lastHTML = null;
  const next = new EventSource('/events?' + new URLSearchParams({repo: repository.value}));
  source = next;
  next.onerror = () => { if (source === next) setStatus('Reconnecting', 'offline'); };
  next.addEventListener('unavailable', () => { if (source === next) setStatus('Data unavailable', 'offline'); });
  next.addEventListener('dashboard', event => {
    if (source !== next) return; // A late message from the previous filter is ignored.
    const data = JSON.parse(event.data);
    knownRepositories = data.repositories;
    updateOptions();
    if (data.html !== lastHTML) {
      const scroll = [...document.querySelectorAll('.cards')].map(lane => lane.scrollTop);
      const focusedRun = document.activeElement?.dataset.run;
      htmx.swap('#board', data.html, {swapStyle: 'outerHTML', settleDelay: 0});
      document.querySelectorAll('.cards').forEach((lane, index) => { lane.scrollTop = scroll[index] || 0; });
      if (focusedRun && !modal.open) document.querySelector(`[data-run="${CSS.escape(focusedRun)}"]`)?.focus({preventScroll: true});
      lastHTML = data.html;
      refreshModal();
    }
    setStatus('Live', 'live');
    tick();
  });
}
repository.addEventListener('change', () => {
  const url = new URL(location.href);
  if (repository.value) url.searchParams.set('repo', repository.value);
  else url.searchParams.delete('repo');
  history.replaceState(null, '', url);
  connect();
});
document.querySelector('.toolbar').addEventListener('submit', event => {event.preventDefault(); connect();});
document.body.addEventListener('click', event => {
  const card = event.target.closest('[data-run]');
  if (!card) return;
  const template = document.getElementById(card.dataset.run);
  if (!template) return;
  selectedRun = card.dataset.run;
  modalContent.replaceChildren(template.content.cloneNode(true));
  modal.showModal();
  tick();
});
document.getElementById('close-modal').addEventListener('click', () => modal.close());
modal.addEventListener('click', event => { if (event.target === modal) { const rect = modal.getBoundingClientRect(); if (event.clientX < rect.left || event.clientX > rect.right || event.clientY < rect.top || event.clientY > rect.bottom) modal.close(); } });
modal.addEventListener('close', () => {document.querySelector(`[data-run="${CSS.escape(selectedRun || '')}"]`)?.focus({preventScroll:true}); selectedRun = null;});
setInterval(tick, 1000);
tick();
connect();

const themeToggle = document.getElementById('theme-toggle');
function updateThemeLabel() {
  const dark = document.documentElement.classList.contains('dark');
  themeToggle.querySelector('[data-theme-label]').textContent = dark ? 'Light mode' : 'Dark mode';
  themeToggle.setAttribute('aria-label', dark ? 'Switch to light mode' : 'Switch to dark mode');
  themeToggle.setAttribute('aria-pressed', String(dark));
}
themeToggle.addEventListener('click', () => {
  const dark = document.documentElement.classList.contains('dark');
  window.dashboardTheme.set(dark ? 'light' : 'dark');
});
document.addEventListener('themechange', updateThemeLabel);
updateThemeLabel();

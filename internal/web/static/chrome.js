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

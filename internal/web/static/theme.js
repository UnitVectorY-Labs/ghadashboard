// Resolve the theme before the stylesheet loads to avoid a theme flash.
window.dashboardTheme = (() => {
  const system = window.matchMedia('(prefers-color-scheme: dark)');
  function savedTheme() {
    const value = document.cookie.split('; ').find(cookie => cookie.startsWith('theme='))?.slice(6);
    return value === 'dark' || value === 'light' ? value : null;
  }
  function apply(theme) {
    document.documentElement.classList.toggle('dark', theme === 'dark');
    document.dispatchEvent(new Event('themechange'));
  }
  function followSystem() {
    apply(savedTheme() || (system.matches ? 'dark' : 'light'));
  }
  system.addEventListener('change', followSystem);
  followSystem();
  return {
    set(theme) {
      document.cookie = `theme=${theme}; Path=/; Max-Age=31536000; SameSite=Lax${location.protocol === 'https:' ? '; Secure' : ''}`;
      apply(theme);
    }
  };
})();

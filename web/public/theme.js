(function () {
  var t = localStorage.getItem('clawvisor_theme');
  var dark = t === 'dark' || (!t && matchMedia('(prefers-color-scheme: dark)').matches);
  var root = document.documentElement;
  if (dark) {
    root.classList.add('dark');
    root.setAttribute('data-mode', 'dark');
  } else {
    root.setAttribute('data-mode', 'light');
  }
  root.setAttribute('data-colorway', 'mono');
  root.setAttribute('data-shape', 'round');
})();

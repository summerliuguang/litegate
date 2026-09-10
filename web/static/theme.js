/* 主题模式:三档 system/light/dark,存 localStorage(data-key=litegate.theme)。
   同步脚本放 head 里样式表 <link> 之前,防夜间模式闪白;API 挂 window.siteTheme。 */
(function () {
  var key = 'litegate.theme';
  function apply(t) {
    var dark = t === 'dark' ||
      (t !== 'light' && window.matchMedia('(prefers-color-scheme: dark)').matches);
    var root = document.documentElement;
    if (dark) root.setAttribute('data-theme', 'dark');
    else root.removeAttribute('data-theme');
    root.style.colorScheme = dark ? 'dark' : 'light';
    /* 样式表加载前先把画布刷成主题底色,避免夜间模式闪白(色值与皮肤 CSS 的 --bg 一致) */
    root.style.background = dark ? '#12141c' : '#f6f7fb';
    root.dataset.themeMode = t;
  }
  window.siteTheme = {
    apply: apply,
    get: function () { try { return localStorage.getItem(key) || 'system'; } catch (e) { return 'system'; } },
    set: function (t) {
      try { localStorage.setItem(key, t); } catch (e) {}
      apply(t);
      document.dispatchEvent(new CustomEvent('themechange', { detail: t }));
    },
    quickToggle: function () {
      var dark = document.documentElement.getAttribute('data-theme') === 'dark';
      this.set(dark ? 'light' : 'dark');
    }
  };
  apply(window.siteTheme.get());
})();

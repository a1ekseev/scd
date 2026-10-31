(() => {
  const tabs = Array.from(document.querySelectorAll('[role="tab"]'));
  const panels = Array.from(document.querySelectorAll('[role="tabpanel"]'));

  const activate = (nextTab, moveFocus) => {
    const selectedIndex = tabs.indexOf(nextTab);
    if (selectedIndex < 0) return;

    tabs.forEach((tab, index) => {
      const selected = index === selectedIndex;
      tab.classList.toggle('is-active', selected);
      tab.setAttribute('aria-selected', String(selected));
      tab.tabIndex = selected ? 0 : -1;
      panels[index].hidden = !selected;
    });

    if (moveFocus) nextTab.focus();
  };

  tabs.forEach((tab, index) => {
    tab.addEventListener('click', () => activate(tab, false));
    tab.addEventListener('keydown', (event) => {
      let nextIndex;
      if (event.key === 'ArrowRight') nextIndex = (index + 1) % tabs.length;
      if (event.key === 'ArrowLeft') nextIndex = (index - 1 + tabs.length) % tabs.length;
      if (event.key === 'Home') nextIndex = 0;
      if (event.key === 'End') nextIndex = tabs.length - 1;
      if (nextIndex === undefined) return;

      event.preventDefault();
      activate(tabs[nextIndex], true);
    });
  });
})();
// Times: the browser's time zone, the ISO value in the title.
document.querySelectorAll('time[datetime]').forEach((el) => {
  const d = new Date(el.getAttribute('datetime'));
  if (Number.isNaN(d.getTime())) return;
  const p = (n) => String(n).padStart(2, '0');
  el.title = el.getAttribute('datetime');
  el.textContent = `${p(d.getDate())}.${p(d.getMonth() + 1)} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
});

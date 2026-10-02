// Small progressive enhancements; every form works without JS.
document.addEventListener('DOMContentLoaded', () => {
  // Confirm destructive actions.
  document.querySelectorAll('form[data-confirm]').forEach(f => {
    f.addEventListener('submit', e => { if (!confirm(f.dataset.confirm)) e.preventDefault(); });
  });
  // Auto-submit selects/inputs marked data-autosubmit.
  document.querySelectorAll('[data-autosubmit]').forEach(el => {
    el.addEventListener('change', () => { if (el.value !== '') el.form.requestSubmit ? el.form.requestSubmit() : el.form.submit(); });
  });
  // Attribute "longer than" needs a minutes box.
  document.querySelectorAll('select[data-attr]').forEach(sel => {
    const mins = sel.form.querySelector('[data-attr-minutes]');
    const sync = () => { mins.hidden = sel.value !== 'longer_than'; mins.required = !mins.hidden; };
    sel.addEventListener('change', sync); sync();
  });
  // History quick actions: scope picker applies to every quick form.
  const scope = document.querySelector('[data-quick-scope]');
  if (scope) {
    let saved = null;
    try { saved = localStorage.getItem('ytg-quick-scope'); } catch (e) {}
    if (saved && [...scope.options].some(o => o.value === saved)) scope.value = saved;
    const apply = () => {
      document.querySelectorAll('form[data-quick] input[name=kid]').forEach(i => { i.value = scope.value; });
      try { localStorage.setItem('ytg-quick-scope', scope.value); } catch (e) {}
    };
    scope.addEventListener('change', apply); apply();
  }
  // Schedule copy helpers.
  const copyRow = (targets) => {
    const rows = [...document.querySelectorAll('table.sched tr')].slice(1);
    const src = [...rows[0].querySelectorAll('input')].map(i => i.value);
    rows.slice(1, targets + 1).forEach(r => r.querySelectorAll('input').forEach((i, n) => { i.value = src[n]; }));
  };
  document.querySelector('[data-copy-first-row]')?.addEventListener('click', () => copyRow(6));
  document.querySelector('[data-copy-weekdays]')?.addEventListener('click', () => copyRow(4));
  // Kid pickers: "All kids" and individual kids exclude each other.
  document.querySelectorAll('input[type=checkbox][name=kid]').forEach(cb => {
    cb.addEventListener('change', () => {
      if (!cb.checked) return;
      cb.form.querySelectorAll('input[type=checkbox][name=kid]').forEach(o => {
        if (o !== cb && (cb.value === 'all') !== (o.value === 'all')) o.checked = false;
      });
    });
  });
  document.querySelectorAll('[data-select]').forEach(i => i.addEventListener('focus', () => i.select()));
});

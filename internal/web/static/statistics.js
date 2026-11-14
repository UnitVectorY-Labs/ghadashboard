// Render only the server snapshot. No background updates.
(() => {
  document.querySelectorAll('.stats-filters, .stats-list-controls').forEach(form => {
    form.addEventListener('change', event => {
      if (form.classList.contains('stats-filters')) {
        if (event.target.name === 'repo') { form.elements.workflow.value = ''; form.elements.worker.value = ''; }
        if (event.target.name === 'workflow') form.elements.worker.value = '';
      }
      form.requestSubmit();
    });
    form.querySelectorAll('input[type=search]').forEach(input => input.addEventListener('search', () => form.requestSubmit()));
  });
  const container = document.querySelector('[data-charts]');
  if (!container) return;
  const data = JSON.parse(container.dataset.charts);
  let charts = [];
  const color = name => getComputedStyle(document.documentElement).getPropertyValue(`--${name}`).trim();
  function render() {
    charts.forEach(chart => chart.destroy());
    charts = [];
    container.querySelectorAll('[data-metrics]').forEach(target => {
      const metrics = target.dataset.metrics.split(',');
      const metric = metrics[0];
      const labels = {runs:'Job runs',errors:'Failed jobs',execution:'Minutes',queue:'Average',p95:'P95',rate:'Failure %'};
      const colors = {runs:'running',errors:'failed',execution:'completed',queue:'queued',p95:'p95',rate:'failed'};
      const series = [{}, ...metrics.map(key => ({
        label: labels[key], stroke: color(colors[key]), width: 2, points: {show: false},
        dash: key === 'p95' ? [5, 3] : [],
        value: (_, value) => value == null ? '—' : key === 'rate' ? `${value.toFixed(1)}%` : ['runs', 'errors'].includes(key) ? String(value) : value.toFixed(1),
      }))];
      charts.push(new uPlot({width: Math.max(200, target.clientWidth), height: 220,
        series, legend: {show: true}, cursor: {drag: {x: false, y: false}},
        scales: {y: {range: (_, min, max) => [0, metric === 'rate' ? 100 : Math.max(1, max * 1.15)]}},
        axes: [{stroke: color('muted'), grid: {stroke: color('border')}, ticks: {stroke: color('border')}},
          {stroke: color('muted'), size: 50, grid: {stroke: color('border')}, ticks: {stroke: color('border')},
            values: (_, ticks) => ticks.map(v => ['runs','errors'].includes(metric) && !Number.isInteger(v) ? '' : Number(v.toFixed(1)).toString())}],
      }, [data.Time, ...metrics.map(key => data.Metrics[key][0])], target));
    });
  }
  render();
  document.addEventListener('themechange', render);
  let width = container.clientWidth;
  new ResizeObserver(() => {
    if (width !== container.clientWidth) {width = container.clientWidth; render();}
  }).observe(container);
})();

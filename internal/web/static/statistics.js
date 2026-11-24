// Render only the server snapshot. No background updates.
(() => {
  if (window.htmx) {
    htmx.config.allowEval = false;
    htmx.config.allowScriptTags = false;
  }

  // Delegate filter and breakdown list controls so HTMX-swapped forms stay wired.
  document.addEventListener('change', event => {
    const form = event.target.closest('.stats-filters, .stats-list-controls');
    if (!form) return;
    if (form.classList.contains('stats-filters')) {
      if (event.target.name === 'repo') { form.elements.workflow.value = ''; form.elements.worker.value = ''; }
      if (event.target.name === 'workflow') form.elements.worker.value = '';
    }
    form.requestSubmit();
  });
  document.addEventListener('search', event => {
    const input = event.target.closest('.stats-filters input[type=search], .stats-list-controls input[type=search]');
    if (!input || !input.form) return;
    input.form.requestSubmit();
  });

  const container = document.querySelector('[data-charts]');
  if (!container) return;
  const data = JSON.parse(container.dataset.charts);
  let charts = [];

  // Resolve light-dark() / var() CSS tokens into concrete rgb() strings for Canvas 2D.
  const probe = document.createElement('span');
  probe.className = 'sr-only';
  probe.setAttribute('aria-hidden', 'true');
  document.body.append(probe);

  function resolveColor(token) {
    probe.style.color = `var(${token})`;
    return getComputedStyle(probe).color;
  }

  function resolveMonoFont() {
    return getComputedStyle(document.documentElement).getPropertyValue('--uvy-font-mono').trim() || 'ui-monospace, monospace';
  }

  const seriesLabels = {
    runs: 'Job runs',
    errors: 'Failed jobs',
    execution: 'Minutes',
    queue: 'Average',
    p95: 'P95',
    rate: 'Failure %',
  };

  const seriesTokens = {
    runs: '--uvy-chart-1',
    errors: '--uvy-danger',
    execution: '--uvy-ok',
    queue: '--uvy-warn',
    p95: '--uvy-chart-4',
    rate: '--uvy-danger',
  };

  function render() {
    charts.forEach(chart => chart.destroy());
    charts = [];
    const axisStroke = resolveColor('--uvy-chart-axis');
    const gridStroke = resolveColor('--uvy-chart-grid');
    const surfaceFill = resolveColor('--uvy-surface');
    const axisFont = `11px ${resolveMonoFont()}`;

    container.querySelectorAll('[data-metrics]').forEach(target => {
      const metrics = target.dataset.metrics.split(',');
      const metric = metrics[0];
      const series = [
        {},
        ...metrics.map(key => {
          const stroke = resolveColor(seriesTokens[key]);
          return {
            label: seriesLabels[key],
            stroke,
            width: 2,
            points: {
              show: false,
              size: 7,
              width: 2,
              stroke,
              fill: surfaceFill,
            },
            dash: key === 'p95' ? [5, 3] : [],
            value: (_, value) =>
              value == null
                ? ''
                : key === 'rate'
                  ? `${value.toFixed(1)}%`
                  : ['runs', 'errors'].includes(key)
                    ? String(value)
                    : value.toFixed(1),
          };
        }),
      ];

      const chart = new uPlot(
          {
            width: Math.max(200, target.clientWidth),
            height: 220,
            series,
            legend: { show: true },
            cursor: {
              drag: { x: false, y: false },
              points: { size: 7, width: 2 },
            },
            scales: {
              y: {
                range: (_, min, max) => [
                  0,
                  metric === 'rate' ? 100 : Math.max(1, max * 1.15),
                ],
              },
            },
            axes: [
              {
                stroke: axisStroke,
                font: axisFont,
                grid: { stroke: gridStroke, width: 1 },
                ticks: { stroke: gridStroke, width: 1, size: 5 },
              },
              {
                stroke: axisStroke,
                font: axisFont,
                size: 52,
                grid: { stroke: gridStroke, width: 1 },
                ticks: { stroke: gridStroke, width: 1, size: 5 },
                values: (_, ticks) =>
                  ticks.map(v =>
                    ['runs', 'errors'].includes(metric) && !Number.isInteger(v)
                      ? ''
                      : Number(v.toFixed(1)).toString(),
                  ),
              },
            ],
          },
          [data.Time, ...metrics.map(key => data.Metrics[key][0])],
          target,
        );
      metrics.forEach((key, idx) => {
        if (key === 'p95') {
          const legendRow = target.querySelectorAll('.u-legend .u-series')[idx + 1];
          if (legendRow) legendRow.classList.add('u-series--dashed');
        }
      });
      charts.push(chart);
    });
  }

  render();

  // Re-apply resolved chart colors whenever data-theme or OS color scheme changes.
  new MutationObserver(render).observe(document.documentElement, {
    attributes: true,
    attributeFilter: ['data-theme'],
  });
  window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', render);

  let width = container.clientWidth;
  new ResizeObserver(() => {
    if (width !== container.clientWidth) {
      width = container.clientWidth;
      render();
    }
  }).observe(container);
})();

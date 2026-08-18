/* Global activity feed.

   Scenarios can be created or edited by an MCP client or by the built-in
   assistant, not just by the person at the keyboard. This subscribes to the
   server's activity stream so those changes surface immediately instead of
   waiting for a manual reload. */

(function () {
  'use strict';

  var esc = window.DL.escapeHTML;
  var lastSeq = 0;
  var source = null;
  // Ignore the backlog on connect: it is history, not news.
  var primed = false;

  function flash(activity) {
    var el = document.createElement('div');
    el.className = 'activity-flash';
    el.innerHTML =
      '<span class="activity-source activity-source-' + esc(activity.source) + '">' +
      esc(activity.source) + '</span>' +
      '<span>' + esc(activity.summary) + '</span>';

    if (activity.scenario_id && shouldOfferReload(activity)) {
      var reload = document.createElement('button');
      reload.className = 'btn btn-sm btn-primary';
      reload.textContent = 'Reload';
      reload.addEventListener('click', function () { window.location.reload(); });
      el.appendChild(reload);
    }

    document.body.appendChild(el);
    setTimeout(function () { el.remove(); }, 9000);
  }

  // A scenario change matters here if this page is showing that scenario, or
  // is a listing of scenarios.
  function shouldOfferReload(activity) {
    var path = window.location.pathname;
    if (path === '/') return true;
    if (path.indexOf('/case/') === 0) {
      return path === '/case/' + activity.scenario_id;
    }
    return false;
  }

  function handle(activity) {
    if (!primed) return;
    switch (activity.kind) {
      case 'scenario.created':
      case 'scenario.updated':
        flash(activity);
        break;
      case 'run.started':
        // Only worth announcing when someone else started it.
        if (activity.source !== 'ui') flash(activity);
        break;
      default:
        break;
    }
    window.dispatchEvent(new CustomEvent('dl-activity', { detail: activity }));
  }

  function receive(activity) {
    if (activity.seq) lastSeq = Math.max(lastSeq, activity.seq);
    handle(activity);
  }

  function connect() {
    if (source) source.close();
    source = new EventSource('/api/activity?since=' + lastSeq);

    source.addEventListener('activity', function (e) {
      var activity;
      try { activity = JSON.parse(e.data); } catch (err) { return; }
      receive(activity);
    });

    source.addEventListener('ready', function () { primed = true; });
    source.addEventListener('error', function () { primed = true; });
  }

  // A save made in this tab already updated this tab; the flash would be noise.
  window.addEventListener('dl-scenario-saved', function () {
    primed = false;
    setTimeout(function () { primed = true; }, 1500);
  });

  // A page that already holds a run's event stream gets its activity from
  // there instead of opening a second one. Browsers allow six connections per
  // origin over HTTP/1.1, and an SSE stream holds one for as long as the page
  // is open — at two per run page, a few open runs exhausted the budget and
  // every other request queued behind them, which looked exactly like runs
  // blocking each other.
  if (window.DL_ACTIVITY_VIA_RUN) {
    window.addEventListener('dl-activity-frame', function (e) { receive(e.detail); });
    // The backlog is history either way, and the run stream does not replay it.
    primed = true;
  } else {
    connect();
  }
})();

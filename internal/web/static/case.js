/* Case page: picking two runs from the history to compare. */

(function () {
  'use strict';

  var button = document.getElementById('compare-selected');
  if (!button) return;

  var picks = Array.prototype.slice.call(document.querySelectorAll('.run-pick'));

  function selected() {
    return picks.filter(function (p) { return p.checked; });
  }

  function update() {
    var chosen = selected();
    button.disabled = chosen.length !== 2;
    button.textContent = chosen.length === 2
      ? 'Compare these two'
      : 'Compare selected (' + chosen.length + '/2)';

    // Once two are ticked, disable the rest so the choice stays unambiguous.
    picks.forEach(function (p) {
      p.disabled = chosen.length >= 2 && !p.checked;
    });
  }

  picks.forEach(function (p) { p.addEventListener('change', update); });
  update();

  button.addEventListener('click', function () {
    var chosen = selected();
    if (chosen.length !== 2) return;
    // The table is newest-first, so the second pick is the older run: show it
    // on the left so the comparison reads oldest to newest.
    window.location.href = '/compare?a=' + encodeURIComponent(chosen[1].value) +
      '&b=' + encodeURIComponent(chosen[0].value);
  });
})();

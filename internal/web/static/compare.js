/* Compare page: filter the step list down to what actually changed. */

(function () {
  'use strict';

  var toggle = document.getElementById('only-diffs');
  var list = document.getElementById('diff-list');
  if (!toggle || !list) return;

  function apply() {
    var onlyDiffs = toggle.checked;
    var shown = 0;
    list.querySelectorAll('.diff-step').forEach(function (step) {
      var changed = step.dataset.changed === '1';
      var visible = changed || !onlyDiffs;
      step.classList.toggle('is-hidden', !visible);
      if (visible) shown++;
    });

    var empty = list.querySelector('.diff-empty');
    if (shown === 0) {
      if (!empty) {
        empty = document.createElement('div');
        empty.className = 'dock-empty diff-empty';
        empty.textContent = 'Every step behaved identically in both runs.';
        list.appendChild(empty);
      }
      empty.hidden = false;
    } else if (empty) {
      empty.hidden = true;
    }
  }

  toggle.addEventListener('change', apply);
  apply();
})();

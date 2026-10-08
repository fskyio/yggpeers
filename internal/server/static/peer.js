(function() {
	'use strict';
	var bar = document.querySelector('.history-bar');
	var detail = document.getElementById('history-detail');
	if (!bar || !detail) return;
	var cells = Array.from(bar.querySelectorAll('.history-hour'));
	function select(cell, focus) {
		cells.forEach(function(other) { other.tabIndex = other === cell ? 0 : -1; });
		detail.textContent = cell.dataset.detail;
		if (focus) cell.focus();
	}
	bar.addEventListener('click', function(event) {
		if (event.target.classList.contains('history-hour')) select(event.target, false);
	});
	bar.addEventListener('focusin', function(event) {
		if (event.target.classList.contains('history-hour')) select(event.target, false);
	});
	bar.addEventListener('pointerover', function(event) {
		if (event.target.classList.contains('history-hour')) detail.textContent = event.target.dataset.detail;
	});
	bar.addEventListener('keydown', function(event) {
		var index = cells.indexOf(event.target);
		if (index < 0) return;
		switch (event.key) {
			case 'ArrowRight': index = Math.min(cells.length - 1, index + 1); break;
			case 'ArrowLeft': index = Math.max(0, index - 1); break;
			case 'Home': index = 0; break;
			case 'End': index = cells.length - 1; break;
			default: return;
		}
		event.preventDefault();
		select(cells[index], true);
	});
})();

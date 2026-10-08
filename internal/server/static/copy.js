(function() {
	'use strict';
	var clipboard = window.isSecureContext && navigator.clipboard && typeof navigator.clipboard.writeText === 'function';
	var legacy = typeof document.execCommand === 'function';
	if (legacy && typeof document.queryCommandSupported === 'function') {
		try { legacy = document.queryCommandSupported('copy'); } catch (_) { legacy = false; }
	}
	if (!clipboard && !legacy) return;

	function legacyCopy(text) {
		if (!legacy) return false;
		var active = document.activeElement;
		var selection = window.getSelection();
		var ranges = [];
		if (selection) {
			for (var i = 0; i < selection.rangeCount; i++) ranges.push(selection.getRangeAt(i).cloneRange());
		}
		var inputSelection = active && typeof active.selectionStart === 'number' ? [active.selectionStart, active.selectionEnd, active.selectionDirection] : null;
		var field = document.createElement('textarea');
		field.className = 'copy-fallback';
		field.value = text;
		field.readOnly = true;
		field.tabIndex = -1;
		field.setAttribute('aria-label', 'Address to copy');
		document.body.appendChild(field);
		var copied = false;
		try {
			field.focus({preventScroll: true});
			field.select();
			field.setSelectionRange(0, field.value.length);
			copied = document.execCommand('copy');
		} catch (_) {
			copied = false;
		} finally {
			field.remove();
			if (active && typeof active.focus === 'function') active.focus({preventScroll: true});
			if (inputSelection) active.setSelectionRange(inputSelection[0], inputSelection[1], inputSelection[2]);
			if (selection) {
				selection.removeAllRanges();
				ranges.forEach(function(range) { selection.addRange(range); });
			}
		}
		return copied;
	}

	document.querySelectorAll('.copy-control').forEach(function(control) {
		var button = control.querySelector('.copy-button');
		var feedback = control.querySelector('.copy-feedback');
		var busy = false;
		var timer;
		button.addEventListener('click', async function(event) {
			event.preventDefault();
			event.stopPropagation();
			if (busy) return;
			busy = true;
			button.setAttribute('aria-busy', 'true');
			clearTimeout(timer);
			feedback.textContent = '';
			control.classList.remove('copied');
			var copied = false;
			if (clipboard) {
				try {
					await navigator.clipboard.writeText(button.dataset.copy);
					copied = true;
				} catch (_) {
					copied = legacyCopy(button.dataset.copy);
				}
			} else {
				copied = legacyCopy(button.dataset.copy);
			}
			feedback.textContent = copied ? 'Copied' : 'Copy failed';
			control.classList.toggle('copied', copied);
			button.title = copied ? 'Address copied' : 'Copy failed. Select the address and copy it manually.';
			button.removeAttribute('aria-busy');
			busy = false;
			timer = setTimeout(function() {
				feedback.textContent = '';
				control.classList.remove('copied');
				button.title = 'Copy address';
			}, copied ? 3000 : 6000);
		});
		control.hidden = false;
	});
})();

// Delegated UI behaviours that CANNOT be inline attributes.
//
// The dashboard's CSP is `script-src 'self'` with no 'unsafe-inline'. That blocks
// inline event-handler ATTRIBUTES (onsubmit=, onchange=) exactly as it blocks
// <script> blocks, which is easy to miss: the CSP comment in middleware.go
// reasoned only about <script> tags. Two consequences, both shipped for months:
//
//   1. Every destructive-action confirm() was an inline onsubmit and therefore
//      never ran. "Delete workflow + all run history", "Erase ALL run history
//      for tenant", "Delete tenant", "Delete user", "Revoke token", "Cancel
//      run", "Overwrite the stored value" were each a single unconfirmed click.
//
//   2. The notification-channel form's per-kind fieldsets are hidden by default
//      and revealed by an inline onchange, so the Slack URL / webhook / SMTP
//      fields never appeared and the form could not be completed AT ALL.
//
// The fix is delegation from an external file, not 'unsafe-inline': relaxing the
// CSP to resurrect the handlers would trade a usability bug for an XSS mitigation
// on a dashboard whose HTML is hand-escaped string concatenation.
(function () {
	'use strict';

	// Confirm before submitting any form carrying data-confirm. Capture phase so
	// it runs ahead of anything else bound to the form.
	document.addEventListener('submit', function (e) {
		var form = e.target;
		if (!form || typeof form.getAttribute !== 'function') return;
		var msg = form.getAttribute('data-confirm');
		if (msg && !window.confirm(msg)) {
			e.preventDefault();
			e.stopPropagation();
		}
	}, true);

	// Reveal the fieldset matching a select's current value. The select carries
	// data-reveal-prefix; each candidate fieldset has class js-reveal-target and
	// id "<prefix><value>".
	function applyReveal(select) {
		var prefix = select.getAttribute('data-reveal-prefix');
		if (!prefix) return;
		var targets = document.getElementsByClassName('js-reveal-target');
		for (var i = 0; i < targets.length; i++) {
			targets[i].hidden = true;
		}
		if (!select.value) return;
		var el = document.getElementById(prefix + select.value);
		if (el) el.hidden = false;
	}

	document.addEventListener('change', function (e) {
		var el = e.target;
		if (el && typeof el.getAttribute === 'function' && el.getAttribute('data-reveal-prefix')) {
			applyReveal(el);
		}
	});

	// Apply once on load so a re-rendered form (a validation error, or the
	// browser restoring a selection on back/forward) shows the right fieldset
	// immediately rather than making the user re-pick the same option.
	document.addEventListener('DOMContentLoaded', function () {
		var selects = document.querySelectorAll('[data-reveal-prefix]');
		for (var i = 0; i < selects.length; i++) {
			applyReveal(selects[i]);
		}
	});
})();

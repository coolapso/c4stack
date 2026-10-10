/*
 * Default page behaviour (a classic deferred script, no build step).
 *
 * Everything is wired through delegated listeners on document, so it also works for
 * markup HTMX swaps in. Pages use these data-* contracts instead of inline scripts:
 *
 * HTMX
 *   Every HTMX request carries the page's CSRF token (meta[name="csrf-token"]) in the
 *   X-CSRF-Token header.
 *   The element that issues a request has aria-busy="true" until the request ends.
 *   When a request gets no response at all, the nearest .dialog-error slot explains
 *   that the change could not be confirmed; otherwise the message is announced.
 *
 * Dialogs (native <dialog class="dialog">, see ui.Dialog)
 *   [data-dialog-open="<id>"]  Opens dialog #<id> and empties its .dialog-error slot.
 *                              Focus returns to the opener when it closes.
 *   [data-dialog-close]        Closes the dialog containing it.
 *   dialog[data-autoshow]      Opens as soon as it is rendered or swapped in.
 *   dialog[data-ephemeral]     When closed, its [data-secret] text is cleared and the
 *                              dialog removed, so a one-time secret leaves the page.
 *   form[data-dialog-reset]    Inside a dialog: reset when the dialog closes.
 *   HX-Trigger: dialog:close   Closes the dialog that issued the request, or
 *                              {"dialog:close": "<id>"} closes dialog #<id>. Servers
 *                              send it on success only, so errors keep the dialog open.
 *   While a request inside a dialog is running, Escape and close buttons are ignored.
 *
 * Copy
 *   [data-copy-target="<id>"]  Copies the text of #<id> and announces it.
 *
 * Busy forms
 *   form[data-busy-form]       On a plain (non-HTMX) submit, marks the form busy and
 *                              disables its submit buttons until the page changes.
 */
(() => {
    'use strict';

    const closest = (target, selector) => (target instanceof Element ? target.closest(selector) : null);

    /* Live region ---------------------------------------------------------- */

    let announceTimer;
    const announce = (message) => {
        const region = document.getElementById('live-status');
        if (!region) return;
        region.textContent = '';
        clearTimeout(announceTimer);
        announceTimer = setTimeout(() => {
            region.textContent = message;
        }, 50);
    };

    /* Dialogs -------------------------------------------------------------- */

    const openers = new WeakMap();
    const autoshown = new WeakSet();
    const isBusy = (dialog) => Boolean(dialog.querySelector('[aria-busy="true"]'));

    const openDialog = (dialog, opener) => {
        if (!(dialog instanceof HTMLDialogElement) || dialog.open) return;
        dialog.querySelectorAll('.dialog-error').forEach((slot) => slot.replaceChildren());
        if (opener) openers.set(dialog, opener);
        dialog.showModal();
    };

    const closeDialog = (dialog, force) => {
        if (!(dialog instanceof HTMLDialogElement) || !dialog.open) return;
        if (!force && isBusy(dialog)) return;
        dialog.close();
    };

    const autoshow = () => {
        document.querySelectorAll('dialog[data-autoshow]').forEach((dialog) => {
            if (autoshown.has(dialog) || !dialog.isConnected) return;
            autoshown.add(dialog);
            openDialog(dialog, document.activeElement instanceof HTMLElement ? document.activeElement : null);
        });
    };

    document.addEventListener('click', (event) => {
        const opener = closest(event.target, '[data-dialog-open]');
        if (opener) {
            event.preventDefault();
            openDialog(document.getElementById(opener.getAttribute('data-dialog-open')), opener);
            return;
        }
        const closer = closest(event.target, '[data-dialog-close]');
        if (closer) {
            event.preventDefault();
            closeDialog(closer.closest('dialog'), false);
        }
    });

    // Escape fires "cancel"; keep a dialog with a running request open.
    document.addEventListener('cancel', (event) => {
        if (event.target instanceof HTMLDialogElement && isBusy(event.target)) event.preventDefault();
    }, true);

    document.addEventListener('close', (event) => {
        const dialog = event.target;
        if (!(dialog instanceof HTMLDialogElement)) return;
        dialog.querySelectorAll('form[data-dialog-reset]').forEach((form) => form.reset());
        const opener = openers.get(dialog);
        openers.delete(dialog);
        if (opener?.isConnected) opener.focus();
        if (dialog.hasAttribute('data-ephemeral')) {
            dialog.querySelectorAll('[data-secret]').forEach((secret) => {
                secret.textContent = '';
            });
            dialog.remove();
        }
    }, true);

    // A page restored from the back/forward cache must not show a one-time secret.
    window.addEventListener('pagehide', () => {
        document.querySelectorAll('dialog[data-ephemeral]').forEach((dialog) => dialog.close());
    });

    let respondingDialog = null;
    document.addEventListener('dialog:close', (event) => {
        const id = event.detail?.value || event.detail?.id;
        const dialog = id ? document.getElementById(id) : closest(event.target, 'dialog') || respondingDialog;
        closeDialog(dialog, true);
    });

    /* Copy ----------------------------------------------------------------- */

    document.addEventListener('click', async (event) => {
        const button = closest(event.target, '[data-copy-target]');
        if (!button) return;
        const source = document.getElementById(button.getAttribute('data-copy-target'));
        if (!source) return;
        const text = source instanceof HTMLInputElement ? source.value : source.textContent.trim();
        try {
            await navigator.clipboard.writeText(text);
            announce('Copied to clipboard.');
        } catch {
            announce('Copying failed. Select the text and copy it instead.');
        }
    });

    /* Busy forms ----------------------------------------------------------- */

    document.addEventListener('submit', (event) => {
        const form = event.target;
        if (!(form instanceof HTMLFormElement) || !form.matches('[data-busy-form]')) return;
        // Let validation and other handlers cancel the submit first.
        setTimeout(() => {
            if (event.defaultPrevented) return;
            form.setAttribute('aria-busy', 'true');
            form.querySelectorAll('button[type="submit"], button:not([type])').forEach((button) => {
                button.disabled = true;
            });
        }, 0);
    });

    window.addEventListener('pageshow', (event) => {
        if (!event.persisted) return;
        document.querySelectorAll('form[data-busy-form][aria-busy]').forEach((form) => {
            form.removeAttribute('aria-busy');
            form.querySelectorAll('button').forEach((button) => {
                button.disabled = false;
            });
        });
    });

    /* HTMX ----------------------------------------------------------------- */

    const source = (event) => event.detail?.ctx?.sourceElement || (event.target instanceof Element ? event.target : null);

    document.addEventListener('htmx:config:request', (event) => {
        const token = document.querySelector('meta[name="csrf-token"]')?.content;
        const request = event.detail?.ctx?.request;
        if (token && request?.headers) request.headers['X-CSRF-Token'] = token;
    });

    document.addEventListener('htmx:before:request', (event) => {
        const element = source(event);
        if (!element) return;
        element.setAttribute('aria-busy', 'true');
        if (event.detail?.ctx) event.detail.ctx.dialog = element.closest('dialog');
    });

    document.addEventListener('htmx:after:request', (event) => {
        respondingDialog = event.detail?.ctx?.dialog || null;
    });

    document.addEventListener('htmx:finally:request', (event) => {
        source(event)?.removeAttribute('aria-busy');
        respondingDialog = null;
    });

    document.addEventListener('htmx:after:settle', autoshow);

    // No response arrived (connection lost, timeout, blocked). The change may or may
    // not have reached the server, so never resubmit: point to a reload instead.
    document.addEventListener('htmx:error', (event) => {
        const ctx = event.detail?.ctx;
        if (!ctx?.request || ctx.response || !(ctx.sourceElement instanceof Element)) return;
        const element = ctx.sourceElement;
        const slot = element.closest('form')?.querySelector('.dialog-error') || element.closest('dialog')?.querySelector('.dialog-error');
        const message = ctx.request.method === 'GET'
            ? 'This section could not load. Check your connection and reload the page.'
            : 'We could not confirm that this went through. Check your connection, then reload the page to see the current state.';
        if (slot?.isConnected) {
            const notice = document.createElement('p');
            notice.className = 'notice danger';
            notice.textContent = message;
            slot.replaceChildren(notice);
        } else {
            announce(message);
        }
    });

    if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', autoshow);
    else autoshow();
})();

/*
 * Passkeys for C4Stack's own sign-in (identity/local). A classic deferred script, no
 * build step, served from the app's own assets.
 *
 * It drives the element with data-local-passkey:
 *   "login"    offers the browser's passkeys in the address field's autofill
 *              (conditional mediation), and behind [data-passkey-start]
 *   "confirm"  asks for the signed-in user's passkey behind [data-passkey-start]
 *   "add"      creates a passkey from form[data-passkey-add]
 * data-options and data-finish are the server's endpoints, data-next where sign-in
 * continues. [data-passkey-only] and form[data-passkey-add] show only when the browser
 * supports passkeys; [data-passkey-unsupported] hides then. Errors go to
 * [data-passkey-error].
 */
(() => {
    'use strict';

    const root = document.querySelector('[data-local-passkey]');
    if (!root || !window.PublicKeyCredential || !navigator.credentials) return;

    const mode = root.dataset.localPasskey;
    const optionsURL = root.dataset.options;
    const finishURL = root.dataset.finish;
    const next = root.dataset.next || '';

    /* Encoding ------------------------------------------------------------- */

    const fromB64url = (s) => {
        const b64 = s.replace(/-/g, '+').replace(/_/g, '/');
        const bin = atob(b64 + '='.repeat((4 - (b64.length % 4)) % 4));
        return Uint8Array.from(bin, (ch) => ch.charCodeAt(0)).buffer;
    };
    const toB64url = (buf) => {
        let bin = '';
        new Uint8Array(buf).forEach((b) => {
            bin += String.fromCharCode(b);
        });
        return btoa(bin).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
    };
    const descriptors = (list) => (list || []).map((c) => ({ ...c, id: fromB64url(c.id) }));

    const requestOptions = (o) => ({
        ...o.publicKey,
        challenge: fromB64url(o.publicKey.challenge),
        allowCredentials: descriptors(o.publicKey.allowCredentials),
    });
    const creationOptions = (o) => ({
        ...o.publicKey,
        challenge: fromB64url(o.publicKey.challenge),
        user: { ...o.publicKey.user, id: fromB64url(o.publicKey.user.id) },
        excludeCredentials: descriptors(o.publicKey.excludeCredentials),
    });
    const extensions = (cred) => (cred.getClientExtensionResults ? cred.getClientExtensionResults() : {});
    const assertion = (cred) => ({
        id: cred.id,
        rawId: toB64url(cred.rawId),
        type: cred.type,
        authenticatorAttachment: cred.authenticatorAttachment || undefined,
        response: {
            clientDataJSON: toB64url(cred.response.clientDataJSON),
            authenticatorData: toB64url(cred.response.authenticatorData),
            signature: toB64url(cred.response.signature),
            userHandle: cred.response.userHandle ? toB64url(cred.response.userHandle) : undefined,
        },
        clientExtensionResults: extensions(cred),
    });
    const attestation = (cred) => ({
        id: cred.id,
        rawId: toB64url(cred.rawId),
        type: cred.type,
        authenticatorAttachment: cred.authenticatorAttachment || undefined,
        response: {
            clientDataJSON: toB64url(cred.response.clientDataJSON),
            attestationObject: toB64url(cred.response.attestationObject),
            transports: cred.response.getTransports ? cred.response.getTransports() : [],
        },
        clientExtensionResults: extensions(cred),
    });

    /* Requests and messages ----------------------------------------------- */

    const csrf = () => {
        const meta = document.querySelector('meta[name="csrf-token"]');
        return meta ? meta.content : '';
    };
    const post = async (url, body) => {
        const res = await fetch(url, {
            method: 'POST',
            credentials: 'same-origin',
            headers: { 'Content-Type': 'application/json', Accept: 'application/json', 'X-CSRF-Token': csrf() },
            body: JSON.stringify(body),
        });
        let data = {};
        try {
            data = await res.json();
        } catch {
            data = {};
        }
        return { ok: res.ok, data };
    };

    const errorSlot = root.querySelector('[data-passkey-error]');
    const showError = (message) => {
        if (!errorSlot) return;
        errorSlot.textContent = message || '';
        errorSlot.hidden = !message;
    };
    // ServerError is a failure the server explained. retry asks for a new ceremony.
    class ServerError extends Error {
        constructor(message, retry) {
            super(message);
            this.name = 'ServerError';
            this.retry = retry;
        }
    }
    const failure = (err) => {
        if (err instanceof ServerError) return err.message;
        if (err && err.name === 'NotAllowedError') return 'The passkey request was cancelled or timed out.';
        if (err && err.name === 'InvalidStateError') return 'This device already has a passkey for your account.';
        return "Your passkey didn't work. Try again, or sign in with a code.";
    };
    const busy = (el, on) => {
        if (!el) return;
        el.setAttribute('aria-busy', on ? 'true' : 'false');
        el.disabled = on;
    };

    root.querySelectorAll('[data-passkey-only]').forEach((el) => {
        el.hidden = false;
    });
    root.querySelectorAll('[data-passkey-unsupported]').forEach((el) => {
        el.hidden = true;
    });

    /* Signing in and confirming ------------------------------------------- */

    // assert runs one ceremony, and goes on to next when it signs in.
    const assert = async (mediation, signal) => {
        const purpose = mode === 'confirm' ? 'confirm' : 'login';
        const options = await post(optionsURL, { purpose, next });
        if (!options.ok) throw new ServerError(options.data.error || 'Passkeys are not available right now.', false);
        const cred = await navigator.credentials.get({
            mediation,
            publicKey: requestOptions(options.data),
            signal,
        });
        if (!cred) return;
        const res = await post(finishURL, assertion(cred));
        if (res.ok && res.data.redirect) {
            window.location.assign(res.data.redirect);
            return;
        }
        throw new ServerError(res.data.error || "Your passkey didn't work. Try again, or sign in with a code.", Boolean(res.data.retry));
    };

    // The address field's autofill offers passkeys. Its ceremony starts again before
    // the challenge expires, and quietly when another tab replaced it.
    let autofill = null;
    let refresh = 0;
    const startAutofill = async () => {
        if (mode !== 'login' || !PublicKeyCredential.isConditionalMediationAvailable) return;
        if (!(await PublicKeyCredential.isConditionalMediationAvailable())) return;
        if (autofill) autofill.abort();
        const controller = new AbortController();
        autofill = controller;
        clearTimeout(refresh);
        refresh = setTimeout(startAutofill, 4 * 60 * 1000);
        try {
            await assert('conditional', controller.signal);
        } catch (err) {
            if (controller.signal.aborted || (err && err.name === 'AbortError')) return;
            if (err instanceof ServerError && err.retry) {
                startAutofill();
                return;
            }
            showError(failure(err));
        }
    };

    const start = root.querySelector('[data-passkey-start]');
    if (start && (mode === 'login' || mode === 'confirm')) {
        start.addEventListener('click', async () => {
            if (autofill) {
                autofill.abort();
                autofill = null;
            }
            showError('');
            busy(start, true);
            try {
                await assert(undefined, undefined);
            } catch (err) {
                showError(failure(err));
            } finally {
                busy(start, false);
                startAutofill();
            }
        });
    }
    startAutofill();

    /* Adding a passkey ---------------------------------------------------- */

    const form = root.querySelector('form[data-passkey-add]');
    if (form && mode === 'add') {
        form.hidden = false;
        const submit = form.querySelector('button[type="submit"]');
        form.addEventListener('submit', async (event) => {
            event.preventDefault();
            const name = form.elements.name.value.trim();
            if (!name) {
                showError('Name the passkey first, such as the device it is on.');
                return;
            }
            showError('');
            busy(submit, true);
            try {
                const options = await post(optionsURL, {});
                if (!options.ok) throw new ServerError(options.data.error || 'Passkeys are not available right now.', false);
                const cred = await navigator.credentials.create({ publicKey: creationOptions(options.data) });
                if (!cred) return;
                const res = await post(finishURL, { name, credential: attestation(cred) });
                if (res.ok && res.data.redirect) {
                    window.location.assign(res.data.redirect);
                    return;
                }
                throw new ServerError(res.data.error || 'The passkey could not be added. Try again.', false);
            } catch (err) {
                showError(failure(err));
            } finally {
                busy(submit, false);
            }
        });
    }
})();

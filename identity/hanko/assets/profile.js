/*
 * Profile UI on the account settings page (ES module).
 *
 * Passkeys (list, add with a name, remove) and signed-in devices (list, sign out) run
 * through Hanko's browser SDK. The email change runs on the server and needs no code
 * here. Markup comes from the Hanko adapter's profileSection and is found through
 * data-* hooks only:
 *
 *   [data-hanko-profile]           data-api-url, data-module (the SDK's URL),
 *                                  data-authorize (asks the server whether the
 *                                  session is recent enough to change passkeys) and
 *                                  data-recent ("true" when it is).
 *   [data-passkey-add]             Enabled once passkeys load.
 *   [data-passkey-list]            Filled from <template data-passkey-row> and
 *                                  <template data-passkey-empty>.
 *   [data-device-list]             Filled from <template data-device-row> and
 *                                  <template data-device-current>.
 *   [data-passkey-status], [data-device-status]   Result messages.
 *   form[data-passkey-add-form], [data-passkey-remove-confirm],
 *   [data-device-sign-out-confirm] Actions inside dialogs.
 *
 * Dialogs are the native <dialog> elements ui.js manages. IP addresses and raw user
 * agents are never shown.
 */

const root = document.querySelector("[data-hanko-profile]");

const dateFormat = new Intl.DateTimeFormat(undefined, { day: "numeric", month: "long", year: "numeric" });
const formatDate = (value) => {
    const date = new Date(value);
    return Number.isNaN(date.getTime()) ? "" : dateFormat.format(date);
};

/* Hanko SDK ---------------------------------------------------------------- */

let sdk;
const loadSDK = () => {
    sdk ??= import(root.dataset.module)
        .then(async (module) => {
            const { hanko } = await module.register(root.dataset.apiUrl, { injectStyles: false });
            return { hanko, webauthn: module.WebauthnSupport.supported() };
        })
        .catch((error) => {
            sdk = undefined;
            throw error;
        });
    return sdk;
};

let state = null;

const loadProfile = async () => {
    const { hanko } = await loadSDK();
    const next = await hanko.createState("profile", { loadFromCache: false, dispatchAfterStateChangeEvent: false });
    if (next.name !== "profile_init" || next.error) throw new Error(`unexpected profile state ${next.name}`);
    state = next;
};

const reloadProfile = async () => {
    try {
        await loadProfile();
    } catch {
        state = null;
    }
};

// run performs one profile action. A state accepts one action, so the page always
// continues from the state the action returns, or from a freshly loaded one.
const run = async (name, input = null) => {
    const action = state?.actions?.[name];
    if (!action?.enabled) throw new Error(`action unavailable: ${name}`);
    let next;
    try {
        next = await action.run(input, { dispatchAfterStateChangeEvent: false });
    } catch (error) {
        await reloadProfile();
        throw error;
    }
    if (next?.name === "profile_init") state = next;
    else await reloadProfile();
    if (!next || next.error || next.name !== "profile_init") throw new Error(`action failed: ${name}`);
    return next;
};

// authorize asks the server whether the session is recent enough to change passkeys.
const authorize = async () => {
    const response = await fetch(root.dataset.authorize, {
        method: "POST",
        credentials: "same-origin",
        redirect: "manual",
        headers: { "X-CSRF-Token": document.querySelector('meta[name="csrf-token"]')?.content ?? "" },
    });
    if (response.status === 204) return true;
    if (response.status === 401 || response.type === "opaqueredirect") return false;
    throw new Error(`authorization answered ${response.status}`);
};

/* Rendering ---------------------------------------------------------------- */

const clone = (selector) => root.querySelector(selector)?.content.firstElementChild?.cloneNode(true);
const recent = () => root.dataset.recent === "true";
const dialogFor = (id) => (recent() ? id : "passkey-reauth-dialog");

const showStatus = (selector, message, tone) => {
    const region = root.querySelector(selector);
    if (!region) return;
    region.replaceChildren();
    if (!message) return;
    const p = document.createElement("p");
    p.className = `notice ${tone}`;
    p.textContent = message;
    region.append(p);
};

const setError = (dialog, message) => {
    const slot = dialog?.querySelector(".dialog-error");
    if (!slot) return;
    slot.replaceChildren();
    if (!message) return;
    const p = document.createElement("p");
    p.className = "notice danger";
    p.textContent = message;
    slot.append(p);
};

const renderPasskeys = () => {
    const list = root.querySelector("[data-passkey-list]");
    const passkeys = state.payload?.user?.passkeys ?? [];
    list.removeAttribute("aria-busy");
    if (passkeys.length === 0) {
        list.replaceChildren(clone("template[data-passkey-empty]"));
        return;
    }
    const removable = state.actions.webauthn_credential_delete?.enabled;
    list.replaceChildren(...passkeys.map((passkey, index) => {
        const row = clone("template[data-passkey-row]");
        const name = passkey.name || `Passkey ${index + 1}`;
        const added = formatDate(passkey.created_at);
        row.querySelector('[data-field="name"]').textContent = name;
        row.querySelector('[data-field="detail"]').textContent = added ? `Added ${added}` : "";
        const remove = row.querySelector("[data-passkey-remove]");
        if (removable) {
            remove.dataset.passkeyId = passkey.id;
            remove.dataset.passkeyName = name;
            remove.setAttribute("aria-label", `Remove passkey ${name}`);
            remove.setAttribute("data-dialog-open", dialogFor("passkey-remove-dialog"));
        } else {
            remove.remove();
        }
        return row;
    }));
};

const renderDevices = () => {
    const list = root.querySelector("[data-device-list]");
    const remove = state.actions.session_delete;
    const allowed = remove?.inputs?.session_id?.allowed_values;
    const deletable = allowed ? new Set(allowed.map((option) => option.value)) : null;
    const sessions = [...(state.payload?.sessions ?? [])].sort((a, b) =>
        Number(Boolean(b.current)) - Number(Boolean(a.current)) ||
        String(b.last_used || "").localeCompare(String(a.last_used || "")));
    list.removeAttribute("aria-busy");
    list.replaceChildren(...sessions.map((session) => {
        const name = session.user_agent || "Signed-in device";
        const row = clone(session.current ? "template[data-device-current]" : "template[data-device-row]");
        row.querySelector('[data-field="name"]').textContent = name;
        if (session.current) return row;
        const lastActive = formatDate(session.last_used);
        row.querySelector('[data-field="detail"]').textContent = lastActive ? `Last active ${lastActive}` : "";
        const signOut = row.querySelector("[data-device-sign-out]");
        if (remove?.enabled && (!deletable || deletable.has(session.id))) {
            signOut.dataset.sessionId = session.id;
            signOut.setAttribute("aria-label", `Sign out ${name}`);
            signOut.setAttribute("data-dialog-open", "device-sign-out-dialog");
        } else {
            signOut.remove();
        }
        return row;
    }));
};

const renderAddButton = (webauthn) => {
    const button = root.querySelector("[data-passkey-add]");
    const canCreate = Boolean(state.actions.webauthn_credential_create?.enabled);
    if (!webauthn) root.querySelector("[data-passkey-unsupported]").hidden = false;
    if (webauthn && canCreate) {
        button.removeAttribute("aria-disabled");
        button.setAttribute("data-dialog-open", dialogFor("passkey-add-dialog"));
    } else {
        button.setAttribute("aria-disabled", "true");
        button.removeAttribute("data-dialog-open");
    }
};

const render = async () => {
    if (!state) {
        root.querySelectorAll("[data-passkey-list], [data-device-list]").forEach((list) => {
            list.removeAttribute("aria-busy");
            list.replaceChildren();
        });
        showStatus("[data-passkey-status]", "Your passkeys could not load. Reload the page to try again.", "danger");
        showStatus("[data-device-status]", "Your signed-in devices could not load. Reload the page to try again.", "danger");
        return false;
    }
    const { webauthn } = await loadSDK();
    renderPasskeys();
    renderDevices();
    renderAddButton(webauthn);
    return true;
};

// busy disables a dialog's controls while its action runs and returns a function
// that restores them.
const busy = (trigger) => {
    const dialog = trigger.closest("dialog");
    const controls = [...(dialog ? dialog.querySelectorAll("button, input") : [trigger])];
    const previous = controls.map((control) => [control, control.disabled]);
    controls.forEach((control) => {
        control.disabled = true;
    });
    trigger.setAttribute("aria-busy", "true");
    return () => {
        previous.forEach(([control, disabled]) => {
            control.disabled = disabled;
        });
        trigger.removeAttribute("aria-busy");
    };
};

const close = (dialog) => {
    if (dialog?.open) dialog.close();
};

// requireSignIn switches passkey controls to the "sign in again" dialog once the
// server says the session is no longer recent.
const requireSignIn = async (dialog) => {
    root.dataset.recent = "false";
    await render();
    close(dialog);
    document.getElementById("passkey-reauth-dialog")?.showModal();
};

/* Actions ------------------------------------------------------------------ */

let pendingPasskey = null;
let pendingSession = null;

const addPasskey = async (form) => {
    const dialog = form.closest("dialog");
    const input = form.elements.namedItem("passkey_name");
    const submit = form.querySelector('button[type="submit"]');
    const name = input.value.trim();
    if (name.length < 3 || name.length > 32) {
        setError(dialog, "Use 3 to 32 characters for the passkey name.");
        input.focus();
        return;
    }
    setError(dialog, "");
    const release = busy(submit);
    try {
        if (!(await authorize())) {
            release();
            await requireSignIn(dialog);
            return;
        }
        const existing = new Set((state.payload?.user?.passkeys ?? []).map((passkey) => passkey.id));
        try {
            await run("webauthn_credential_create");
        } catch {
            await render();
            release();
            setError(dialog, "The passkey was not added. Try again when your device is ready.");
            return;
        }
        // Name the new passkey. If that fails, it keeps Hanko's default name.
        let named = false;
        const created = (state.payload?.user?.passkeys ?? []).find((passkey) => !existing.has(passkey.id));
        if (created) {
            try {
                await run("webauthn_credential_rename", { passkey_id: created.id, passkey_name: name });
                named = true;
            } catch {
                named = false;
            }
        }
        await render();
        release();
        close(dialog);
        if (named) showStatus("[data-passkey-status]", "Passkey added.", "success");
        else showStatus("[data-passkey-status]", "Passkey added. Its name could not be saved, so it shows a default name.", "warning");
    } catch {
        release();
        setError(dialog, "The passkey could not be added. Try again.");
    }
};

const removePasskey = async (button) => {
    const dialog = button.closest("dialog");
    if (!pendingPasskey) {
        close(dialog);
        return;
    }
    setError(dialog, "");
    const release = busy(button);
    try {
        if (!(await authorize())) {
            release();
            await requireSignIn(dialog);
            return;
        }
        await run("webauthn_credential_delete", { passkey_id: pendingPasskey.id });
        pendingPasskey = null;
        await render();
        release();
        close(dialog);
        showStatus("[data-passkey-status]", "Passkey removed.", "success");
    } catch {
        await render();
        release();
        setError(dialog, "The passkey could not be removed. Try again.");
    }
};

const signOutDevice = async (button) => {
    const dialog = button.closest("dialog");
    if (!pendingSession) {
        close(dialog);
        return;
    }
    setError(dialog, "");
    const release = busy(button);
    try {
        await run("session_delete", { session_id: pendingSession });
        pendingSession = null;
        await render();
        release();
        close(dialog);
        showStatus("[data-device-status]", "Device signed out.", "success");
    } catch {
        await render();
        release();
        setError(dialog, "That device could not be signed out. Try again.");
    }
};

/* Wiring ------------------------------------------------------------------- */

document.addEventListener("click", (event) => {
    const target = event.target instanceof Element ? event.target : null;
    if (!target) return;
    const remove = target.closest("[data-passkey-remove]");
    if (remove) {
        pendingPasskey = { id: remove.dataset.passkeyId, name: remove.dataset.passkeyName };
        const label = document.querySelector("[data-passkey-remove-name]");
        if (label) label.textContent = pendingPasskey.name || "This passkey";
        return;
    }
    const signOut = target.closest("[data-device-sign-out]");
    if (signOut) {
        pendingSession = signOut.dataset.sessionId || null;
        return;
    }
    const confirmRemove = target.closest("[data-passkey-remove-confirm]");
    if (confirmRemove) {
        removePasskey(confirmRemove);
        return;
    }
    const confirmSignOut = target.closest("[data-device-sign-out-confirm]");
    if (confirmSignOut) signOutDevice(confirmSignOut);
});

document.addEventListener("submit", (event) => {
    const form = event.target;
    if (form instanceof HTMLFormElement && form.matches("[data-passkey-add-form]")) {
        event.preventDefault();
        addPasskey(form);
    }
}, true);

if (root) {
    await reloadProfile();
    try {
        await render();
    } catch {
        state = null;
        await render().catch(() => {});
    }
}

// Sign-in page (ES module). Registers Hanko's <hanko-auth> element (#hanko-auth) with
// the API URL in its data-api-url attribute, and once Hanko creates a session continues
// to the local path in data-next. If Hanko cannot load, #login-error is revealed.

const auth = document.getElementById("hanko-auth");

const localPath = (path) =>
    typeof path === "string" && path.startsWith("/") && !path.startsWith("//") && !path.startsWith("/\\") ? path : "/";

const showError = () => {
    const message = document.getElementById("login-error");
    if (message) message.hidden = false;
};

if (auth?.dataset.apiUrl && auth.dataset.module) {
    try {
        const { register } = await import(auth.dataset.module);
        const { hanko } = await register(auth.dataset.apiUrl);
        hanko.onSessionCreated(() => window.location.assign(localPath(auth.dataset.next)));
    } catch {
        showError();
    }
} else if (auth) {
    showError();
}

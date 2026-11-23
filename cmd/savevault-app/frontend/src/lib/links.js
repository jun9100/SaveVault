// Every destination the app can send someone to outside itself, in one file.
// A link that leaves the app is one worth being able to audit at a glance
// rather than hunting for a string literal in a view.
//
// All of these are opened with native.openExternal — in the system browser,
// never inside the app window.

export const GITHUB_URL = 'https://github.com/jun9100/SaveVault';

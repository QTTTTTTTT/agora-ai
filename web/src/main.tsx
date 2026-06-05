import React from 'react'
import ReactDOM from 'react-dom/client'
import App from './App'
import './index.css'
import { PreferencesProvider } from './lib/preferences'
import { ThemeProvider } from './lib/theme'

// Vite emits a `vite:preloadError` window event when a chunk referenced
// from a `<link rel="modulepreload">` tag (i.e. the entry-time preloads,
// not user-triggered dynamic imports) fails to load. This is the same
// underlying class of failure as "Failed to fetch dynamically imported
// module" but it can fire *before* React even mounts, so the
// `lazyWithRetry` wrapper has no chance to catch it.
//
// The standard remediation — and what Vite explicitly documents the event
// for — is to reload the page so the browser picks up the fresh
// `index.html` and the fresh chunk hashes. Without this hook a user whose
// network blipped during initial load just sits there staring at a blank
// page forever.
//
// We guard with a sessionStorage flag so a genuinely broken bundle can't
// trap the user in an infinite reload loop: at most one reload per tab.
const PRELOAD_RELOAD_FLAG = 'fundai:preload-reload-attempted'
window.addEventListener('vite:preloadError', (event) => {
  try {
    const tried = sessionStorage.getItem(PRELOAD_RELOAD_FLAG)
    if (tried) return
    sessionStorage.setItem(PRELOAD_RELOAD_FLAG, String(Date.now()))
  } catch {
    // sessionStorage may be unavailable (3rd-party-cookie-blocked iframes,
    // disk full, …). Reloading once anyway is still strictly better than
    // a permanently blank page.
  }
  // Prevent the default error from being thrown so React doesn't try to
  // render an error boundary in the millisecond before we navigate.
  event.preventDefault()
  window.location.reload()
})

ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <ThemeProvider>
      <PreferencesProvider>
        <App />
      </PreferencesProvider>
    </ThemeProvider>
  </React.StrictMode>,
)

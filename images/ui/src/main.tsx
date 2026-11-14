import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
// self-hosted fonts: the CSP allows styles and fonts from this origin only
import '@fontsource/archivo/400'
import '@fontsource/archivo/500'
import '@fontsource/archivo/600'
import '@fontsource/archivo/700'
import '@fontsource/big-shoulders-display/700'
import '@fontsource/big-shoulders-display/800'
import '@fontsource/martian-mono/400'
import '@fontsource/martian-mono/500'
import './index.css'
import { App } from './App.tsx'
import { loadConfig } from './config'
import { userManager } from './auth'

// the theme the person chose (light or dark); without one the system setting applies
try {
  const t = localStorage.getItem('ad-theme')
  if (t === 'light' || t === 'dark') document.documentElement.dataset.theme = t
} catch { /* storage blocked */ }

const root = createRoot(document.getElementById('root')!)
loadConfig()
  .then((cfg) => root.render(<StrictMode><App um={userManager(cfg)} /></StrictMode>))
  .catch((e) => root.render(<p className="ad-error" style={{ padding: 16 }}>agentdev UI cannot start: {String(e.message ?? e)}</p>))

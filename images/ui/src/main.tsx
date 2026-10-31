import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import './index.css'
import { App } from './App.tsx'
import { loadConfig } from './config'
import { userManager } from './auth'

const root = createRoot(document.getElementById('root')!)
loadConfig()
  .then((cfg) => root.render(<StrictMode><App um={userManager(cfg)} /></StrictMode>))
  .catch((e) => root.render(<p className="error">agentdev UI cannot start: {String(e.message ?? e)}</p>))

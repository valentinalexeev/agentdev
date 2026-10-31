// Runtime configuration, served by nginx as /config.json (rendered from the pod's environment), so one image
// works on kind and on GKE.
export interface Config {
  oidcIssuer: string // the URL users reach authentik through, e.g. https://localhost:9443/application/o/agentdev/
  oidcClientId: string
}

export async function loadConfig(): Promise<Config> {
  const r = await fetch('/config.json', { cache: 'no-store' })
  if (!r.ok) throw new Error(`config.json: ${r.status}`)
  return r.json()
}

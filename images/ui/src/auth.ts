import { UserManager, WebStorageStateStore, type User } from 'oidc-client-ts'
import type { Config } from './config'

// authentik login: authorization code + PKCE with the public client `agentdev` (created by the authentik blueprint).
// Tokens stay in sessionStorage (closed tab = logged out); the API checks every call.
export function userManager(cfg: Config): UserManager {
  return new UserManager({
    authority: cfg.oidcIssuer,
    client_id: cfg.oidcClientId,
    redirect_uri: `${window.location.origin}/callback`,
    post_logout_redirect_uri: `${window.location.origin}/`,
    scope: 'openid profile email',
    userStore: new WebStorageStateStore({ store: window.sessionStorage }),
    automaticSilentRenew: false,
  })
}

export type { User, UserManager }

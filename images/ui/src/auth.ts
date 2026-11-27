import { UserManager, WebStorageStateStore, type User } from 'oidc-client-ts'
import type { Config } from './config'

// authentik login: authorization code + PKCE with the public client `agentdev` (created by the authentik blueprint).
// Tokens stay in sessionStorage (closed tab = logged out); the API checks every call.
export function userManager(cfg: Config): UserManager {
  return new UserManager({
    authority: cfg.oidcIssuer,
    client_id: cfg.oidcClientId || 'agentdev',
    redirect_uri: `${window.location.origin}/callback`,
    post_logout_redirect_uri: `${window.location.origin}/`,
    // forge: forge_login claim for the API (#88); providers that issue groups only on request add `groups` (#323)
    scope: cfg.oidcScopes || 'openid profile email forge',
    userStore: new WebStorageStateStore({ store: window.sessionStorage }),
    automaticSilentRenew: false,
  })
}

export type { User, UserManager }

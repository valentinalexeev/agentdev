// The console data (DataProvider in data.tsx) as a context hook.
import { createContext, useContext } from 'react'
import type { SecurityEvent, Approval, Usage } from './api'
import type { AgentView, InboxItem, Scope } from './model'

export type Conn = 'live' | 'stale' | 'starting' | 'down' | 'offline' | 'signedout'


export interface Data {
  ready: boolean
  at: number // time of the last good update
  agents: AgentView[] // in scope
  allAgents: AgentView[]
  pending: Approval[] // in scope
  inbox: InboxItem[] // in scope
  hiddenInbox: number // items outside the scope
  usage: Usage | null
  events: SecurityEvent[] // last 7 days, in scope
  projects: string[] // in scope
  allProjects: string[]
  scope: Scope
  setScope: (s: Scope) => void
  inScopeRepo: (repo: string) => boolean
  inScopeAgent: (name: string) => boolean
  mine: string[] | null // null: the API does not say which projects are mine
  conn: Conn
  failedSince: number
  reload: () => void
  now: number // the clock of the last render tick (10 s)
}

export const Ctx = createContext<Data | null>(null)

export function useData(): Data {
  const d = useContext(Ctx)
  if (!d) throw new Error('useData outside DataProvider')
  return d
}


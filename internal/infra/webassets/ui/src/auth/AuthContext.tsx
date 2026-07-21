// AuthContext is the SPA's authentication-mode switch. Bootstrap decides whether
// this deployment uses Go's builtin session/CSRF login or a preceding external
// IAP. Only builtin mode probes /api/session and exposes login/logout controls.

import { createContext, useContext, useEffect, useState, useCallback } from 'react'
import type { ReactNode } from 'react'
import * as api from '../lib/api'
import type { DashboardAuthMode } from '../lib/types'

interface AuthState {
  authMode: DashboardAuthMode | null
  user: string | null
  ready: boolean // true once bootstrap (and builtin session recovery) resolves
}

interface AuthContextValue extends AuthState {
  login: (user: string, password: string) => Promise<void>
  logout: () => Promise<void>
}

const AuthContext = createContext<AuthContextValue | null>(null)

export function AuthProvider({ children }: { children: ReactNode }) {
  const [state, setState] = useState<AuthState>({ authMode: null, user: null, ready: false })

  const handleUnauthorized = useCallback(() => {
    setState((s) => (s.authMode !== 'builtin' || s.user === null ? s : { ...s, user: null }))
  }, [])

  useEffect(() => {
    api.setUnauthorizedHandler(handleUnauthorized)
    let cancelled = false
    api
      .getBootstrap()
      .then(async (bootstrap) => {
        api.setDashboardAuthMode(bootstrap.authMode)
        if (bootstrap.authMode === 'external') {
          if (!cancelled) setState({ authMode: 'external', user: null, ready: true })
          return
        }
        try {
          const session = await api.recoverSession()
          if (!cancelled) setState({ authMode: 'builtin', user: session.user, ready: true })
        } catch {
          if (!cancelled) setState({ authMode: 'builtin', user: null, ready: true })
        }
      })
      .catch(() => {
        // A bootstrap failure is not an authentication decision: leave the app
        // unavailable rather than accidentally treating it as external access.
        if (!cancelled) setState({ authMode: null, user: null, ready: true })
      })
    return () => {
      cancelled = true
    }
  }, [handleUnauthorized])

  const login = useCallback(async (user: string, password: string) => {
    const res = await api.login(user, password)
    setState({ authMode: 'builtin', user: res.user, ready: true })
  }, [])

  const logout = useCallback(async () => {
    try {
      await api.logout()
    } finally {
      setState({ authMode: 'builtin', user: null, ready: true })
    }
  }, [])

  return (
    <AuthContext.Provider value={{ ...state, login, logout }}>{children}</AuthContext.Provider>
  )
}

// eslint-disable-next-line react-refresh/only-export-components
export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext)
  if (!ctx) throw new Error('useAuth must be used within AuthProvider')
  return ctx
}

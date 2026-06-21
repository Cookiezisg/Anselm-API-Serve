// AuthContext — the SPA's single source of truth for "am I logged in".
//
// On mount it probes GET /api/session (F5 recovery: a live HttpOnly cookie
// rehydrates the in-memory CSRF token without a re-login). A 401 there simply
// means "not logged in" → the guard routes to the login page. It also registers
// the api layer's onUnauthorized hook so ANY later 401 (session expiry mid-use)
// drops auth state and bounces to login.
//
// 启动探 /api/session 做 F5 恢复;注册 401 钩子,任何会话失效都清态跳登录。

import { createContext, useContext, useEffect, useState, useCallback } from 'react'
import type { ReactNode } from 'react'
import * as api from '../lib/api'

interface AuthState {
  user: string | null
  ready: boolean // true once the initial session probe has resolved
}

interface AuthContextValue extends AuthState {
  login: (user: string, password: string) => Promise<void>
  logout: () => Promise<void>
}

const AuthContext = createContext<AuthContextValue | null>(null)

export function AuthProvider({ children }: { children: ReactNode }) {
  const [state, setState] = useState<AuthState>({ user: null, ready: false })

  const handleUnauthorized = useCallback(() => {
    setState((s) => (s.user === null ? s : { ...s, user: null }))
  }, [])

  useEffect(() => {
    api.setUnauthorizedHandler(handleUnauthorized)
    let cancelled = false
    api
      .recoverSession()
      .then((res) => {
        if (!cancelled) setState({ user: res.user, ready: true })
      })
      .catch(() => {
        // 401 (no/expired session) or any error → unauthenticated, but ready.
        if (!cancelled) setState({ user: null, ready: true })
      })
    return () => {
      cancelled = true
    }
  }, [handleUnauthorized])

  const login = useCallback(async (user: string, password: string) => {
    const res = await api.login(user, password)
    setState({ user: res.user, ready: true })
  }, [])

  const logout = useCallback(async () => {
    try {
      await api.logout()
    } finally {
      setState({ user: null, ready: true })
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

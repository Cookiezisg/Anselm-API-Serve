// Login page — POST /login {user,password}. On the brute-force lockout (429
// LOGIN_LOCKED) the backend returns retryAfterSec in details; we surface a live
// countdown so the operator sees the real (exponentially-backed-off) wait rather
// than a hardcoded number.
//
// 登录页;锁定时读 details.retryAfterSec 做真实倒计时(非写死)。

import { useEffect, useState } from 'react'
import { Card, Form, Input, Button, Typography, Alert } from 'antd'
import { UserOutlined, LockOutlined } from '@ant-design/icons'
import { useNavigate, useLocation } from 'react-router-dom'
import { useAuth } from '../auth/AuthContext'
import { ApiError } from '../lib/api'

const { Title, Text } = Typography

interface LocationState {
  from?: string
}

export default function Login() {
  const { login, user } = useAuth()
  const nav = useNavigate()
  const loc = useLocation()
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [lockSec, setLockSec] = useState(0)

  // Already authenticated → leave the login screen.
  useEffect(() => {
    if (user) {
      const from = (loc.state as LocationState | null)?.from
      nav(from && from !== '/login' ? from : '/overview', { replace: true })
    }
  }, [user, nav, loc.state])

  // Lockout countdown tick.
  useEffect(() => {
    if (lockSec <= 0) return
    const t = setInterval(() => setLockSec((s) => Math.max(0, s - 1)), 1000)
    return () => clearInterval(t)
  }, [lockSec])

  const onFinish = async (vals: { user: string; password: string }) => {
    setSubmitting(true)
    setError(null)
    try {
      await login(vals.user, vals.password)
      // navigation handled by the effect above once user is set
    } catch (e) {
      if (e instanceof ApiError) {
        if (e.code === 'LOGIN_LOCKED') {
          const sec = Number((e.details?.retryAfterSec as number) ?? 0)
          setLockSec(sec > 0 ? sec : 30)
          setError(`尝试过于频繁，请在 ${sec > 0 ? sec : 30} 秒后重试`)
        } else {
          setError(e.message || '登录失败')
        }
      } else {
        setError('登录失败')
      }
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <div
      style={{
        minHeight: '100vh',
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'center',
        background: '#f5f5f7',
      }}
    >
      <Card style={{ width: 380, boxShadow: '0 4px 24px rgba(0,0,0,0.06)' }}>
        <div style={{ textAlign: 'center', marginBottom: 24 }}>
          <Title level={3} style={{ marginBottom: 4, color: '#4f46e5' }}>
            Anselm Gateway
          </Title>
          <Text type="secondary">管理后台登录</Text>
        </div>
        {error && (
          <Alert type="error" showIcon message={error} style={{ marginBottom: 16 }} closable onClose={() => setError(null)} />
        )}
        <Form layout="vertical" onFinish={onFinish} disabled={submitting}>
          <Form.Item name="user" rules={[{ required: true, message: '请输入用户名' }]}>
            <Input prefix={<UserOutlined />} placeholder="用户名" autoComplete="username" size="large" />
          </Form.Item>
          <Form.Item name="password" rules={[{ required: true, message: '请输入密码' }]}>
            <Input.Password prefix={<LockOutlined />} placeholder="密码" autoComplete="current-password" size="large" />
          </Form.Item>
          <Form.Item style={{ marginBottom: 0 }}>
            <Button
              type="primary"
              htmlType="submit"
              block
              size="large"
              loading={submitting}
              disabled={submitting || lockSec > 0}
            >
              {lockSec > 0 ? `锁定中 ${lockSec}s` : '登录'}
            </Button>
          </Form.Item>
        </Form>
      </Card>
    </div>
  )
}

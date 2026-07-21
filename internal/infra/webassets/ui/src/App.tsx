// App shell — the dashboard chrome plus the authentication-mode aware route
// guard. builtin uses the Go login page; external relies on its preceding IAP.

import { Layout, Menu, Typography, theme, Button, Spin, Space } from 'antd'
import {
  DashboardOutlined,
  TeamOutlined,
  SettingOutlined,
  FileTextOutlined,
  DownloadOutlined,
  LogoutOutlined,
} from '@ant-design/icons'
import { Routes, Route, Navigate, useLocation, useNavigate } from 'react-router-dom'
import type { ReactNode } from 'react'
import { useAuth } from './auth/AuthContext'
import Login from './pages/Login'
import Overview from './pages/Overview'
import Installs from './pages/Installs'
import Config from './pages/Config'
import Audit from './pages/Audit'
import Export from './pages/Export'

const { Header, Sider, Content } = Layout

const NAV = [
  { key: '/overview', icon: <DashboardOutlined />, label: '总览' },
  { key: '/installs', icon: <TeamOutlined />, label: 'Installs' },
  { key: '/config', icon: <SettingOutlined />, label: '配置' },
  { key: '/audit', icon: <FileTextOutlined />, label: '审计' },
  { key: '/export', icon: <DownloadOutlined />, label: '导出' },
]

// RequireAuth gates the authenticated shell: it waits for the session probe to
// settle (ready), then either renders children or redirects to /login (carrying
// the attempted path so login can bounce back).
function RequireAuth({ children }: { children: ReactNode }) {
  const { user, ready, authMode } = useAuth()
  const loc = useLocation()
  if (!ready) {
    return (
      <div style={{ minHeight: '100vh', display: 'flex', alignItems: 'center', justifyContent: 'center' }}>
        <Spin size="large" />
      </div>
    )
  }
  if (authMode === 'external') {
    return <>{children}</>
  }
  if (!user) {
    return <Navigate to="/login" replace state={{ from: loc.pathname }} />
  }
  return <>{children}</>
}

function Shell() {
  const loc = useLocation()
  const nav = useNavigate()
  const { token } = theme.useToken()
  const { authMode, user, logout } = useAuth()

  const onLogout = async () => {
    await logout()
    nav('/login', { replace: true })
  }

  return (
    <Layout style={{ minHeight: '100vh' }}>
      <Sider
        theme="light"
        breakpoint="lg"
        collapsedWidth={0}
        style={{ borderRight: `1px solid ${token.colorBorderSecondary}` }}
      >
        <div
          style={{
            height: 56,
            display: 'flex',
            alignItems: 'center',
            paddingLeft: 20,
            fontWeight: 600,
            fontSize: 15,
            color: token.colorPrimary,
          }}
        >
          Anselm Gateway
        </div>
        <Menu
          mode="inline"
          selectedKeys={[loc.pathname]}
          items={NAV}
          onClick={(e) => nav(e.key)}
          style={{ borderInlineEnd: 0 }}
        />
      </Sider>
      <Layout>
        <Header
          style={{
            background: token.colorBgContainer,
            paddingInline: 24,
            borderBottom: `1px solid ${token.colorBorderSecondary}`,
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'space-between',
          }}
        >
          <Typography.Text strong>管理后台</Typography.Text>
          {authMode === 'builtin' && (
            <Space>
              <Typography.Text type="secondary">{user}</Typography.Text>
              <Button icon={<LogoutOutlined />} onClick={onLogout} size="small">
                登出
              </Button>
            </Space>
          )}
        </Header>
        <Content
          style={{
            margin: 24,
            padding: 24,
            background: token.colorBgContainer,
            borderRadius: token.borderRadiusLG,
          }}
        >
          <Routes>
            <Route path="/" element={<Navigate to="/overview" replace />} />
            <Route path="/overview" element={<Overview />} />
            <Route path="/installs" element={<Installs />} />
            <Route path="/config" element={<Config />} />
            <Route path="/audit" element={<Audit />} />
            <Route path="/export" element={<Export />} />
            <Route path="*" element={<Navigate to="/overview" replace />} />
          </Routes>
        </Content>
      </Layout>
    </Layout>
  )
}

export default function App() {
  const { authMode, ready } = useAuth()
  return (
    <Routes>
      <Route
        path="/login"
        element={ready && authMode === 'external' ? <Navigate to="/overview" replace /> : <Login />}
      />
      <Route
        path="/*"
        element={
          <RequireAuth>
            <Shell />
          </RequireAuth>
        }
      />
    </Routes>
  )
}

// App shell — the dashboard chrome and its routes.
//
// There is no route guard and no login page, because this SPA is only ever
// served by a loopback-bound listener sitting behind an IAP. A second, weaker
// gate rendered in the browser would not add a boundary; it would add a thing
// that can disagree with the real one.
//
// 这里没有路由守卫、也没有登录页,因为本 SPA 只会由一个坐在 IAP 后面的 loopback 监听器送出。
// 在浏览器里再画一道更弱的门,不会多出一条边界——只会多出一样可能与真边界说法不一致的东西。

import { Layout, Menu, Typography, theme } from 'antd'
import {
  DashboardOutlined,
  TeamOutlined,
  SettingOutlined,
  FileTextOutlined,
  DownloadOutlined,
} from '@ant-design/icons'
import { Routes, Route, Navigate, useLocation, useNavigate } from 'react-router-dom'
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

export default function App() {
  const loc = useLocation()
  const nav = useNavigate()
  const { token } = theme.useToken()

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

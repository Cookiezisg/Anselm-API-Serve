import React from 'react'
import ReactDOM from 'react-dom/client'
import { ConfigProvider, App as AntdApp, theme } from 'antd'
import zhCN from 'antd/locale/zh_CN'
import { HashRouter } from 'react-router-dom'
import App from './App'
import { AuthProvider } from './auth/AuthContext'
import './index.css'

// HashRouter:dashboard 是 SPA,server 只 serve index.html + 静态资源,路由
// (#/overview 等)全前端处理,无需后端 SPA fallback。AntdApp 提供 message/
// notification/Modal 的静态方法上下文(贴合 ConfigProvider 主题)。
ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <ConfigProvider
      locale={zhCN}
      theme={{
        algorithm: theme.defaultAlgorithm,
        token: { colorPrimary: '#4f46e5', borderRadius: 8 },
      }}
    >
      <AntdApp>
        <HashRouter>
          <AuthProvider>
            <App />
          </AuthProvider>
        </HashRouter>
      </AntdApp>
    </ConfigProvider>
  </React.StrictMode>,
)

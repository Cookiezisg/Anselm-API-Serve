// Export page — GET /api/export streams a consistent SQLite snapshot as an
// attachment (Content-Disposition). A same-origin anchor click carries the
// HttpOnly session cookie and lets the browser handle the download (the server
// sets the filename), so no fetch/blob plumbing is needed. The export is GET-only
// and requires no CSRF (read-only); the backend audits every attempt.
//
// 导出:同源 a 链接点击带 session cookie,浏览器按 Content-Disposition 下载快照;
// GET 无需 CSRF;后端对每次尝试落审计。

import { useState } from 'react'
import { Card, Button, Typography, Space, Alert } from 'antd'
import { DownloadOutlined } from '@ant-design/icons'
import { exportUrl } from '../lib/api'

const { Title, Text, Paragraph } = Typography

export default function Export() {
  const [clicked, setClicked] = useState(false)

  const onDownload = () => {
    // Programmatic anchor click: same-origin GET, cookie rides automatically, the
    // browser saves the attachment using the server's Content-Disposition filename.
    const a = document.createElement('a')
    a.href = exportUrl
    a.rel = 'noopener'
    document.body.appendChild(a)
    a.click()
    document.body.removeChild(a)
    setClicked(true)
  }

  return (
    <div>
      <Title level={3} style={{ marginTop: 0 }}>
        导出
      </Title>
      <Card style={{ maxWidth: 640 }}>
        <Space direction="vertical" size="middle" style={{ width: '100%' }}>
          <Paragraph>
            导出当前数据库的<Text strong>一致快照</Text>（SQLite <Text code>VACUUM INTO</Text>，含 WAL
            已提交状态）。快照只含落盘内容（installs / usage / budget / ledger / settings），
            <Text strong>绝不含任何内存机密</Text>（DeepSeek key / 后台口令在 env + 进程内存，从不入库）。
          </Paragraph>
          <Button type="primary" size="large" icon={<DownloadOutlined />} onClick={onDownload}>
            下载数据库快照
          </Button>
          {clicked && (
            <Alert
              type="info"
              showIcon
              message="已触发下载。文件名形如 anselm-gateway-<时间戳>.db；若浏览器未弹出保存框，请检查弹窗/下载拦截。"
            />
          )}
        </Space>
      </Card>
    </div>
  )
}

// Installs page — GET /api/installs (cursor pagination), with ban (reason-bearing
// modal) / unban (popconfirm). The backend cursor is an opaque offset string and
// the response carries nextCursor only when an older page exists; we keep a stack
// of visited cursors so the operator can page forward and back.
//
// Installs:游标分页表 + 封禁(带 reason 弹窗)/解封(Popconfirm);维护游标栈支持前后翻页。

import { useCallback, useEffect, useState } from 'react'
import {
  Table,
  Button,
  Tag,
  Space,
  Typography,
  Modal,
  Input,
  Popconfirm,
  Alert,
  App,
} from 'antd'
import { ReloadOutlined, StopOutlined, CheckCircleOutlined } from '@ant-design/icons'
import type { ColumnsType } from 'antd/es/table'
import type { InstallRow } from '../lib/types'
import { getInstalls, banInstall, unbanInstall, ApiError } from '../lib/api'
import { formatMicroUsd } from '../lib/money'

const { Title, Text } = Typography
const PAGE_SIZE = 50

function statusTag(status: string) {
  switch (status) {
    case 'active':
      return <Tag color="green">active</Tag>
    case 'banned':
      return <Tag color="red">banned</Tag>
    default:
      return <Tag>{status}</Tag>
  }
}

export default function Installs() {
  const { message } = App.useApp()
  const [rows, setRows] = useState<InstallRow[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)

  // Cursor stack: stack[i] is the cursor that produced page i. The current page's
  // cursor is stack[stack.length-1]; nextCursor (when present) appends a new page.
  const [stack, setStack] = useState<string[]>([''])
  const [nextCursor, setNextCursor] = useState('')

  // Ban modal state.
  const [banTarget, setBanTarget] = useState<InstallRow | null>(null)
  const [banReason, setBanReason] = useState('')
  const [banSubmitting, setBanSubmitting] = useState(false)

  const load = useCallback(
    async (cursor: string) => {
      setLoading(true)
      setError(null)
      try {
        const res = await getInstalls(cursor, PAGE_SIZE)
        setRows(res.installs ?? [])
        setNextCursor(res.nextCursor ?? '')
      } catch (e) {
        if (e instanceof ApiError && e.status !== 401) setError(e.message)
      } finally {
        setLoading(false)
      }
    },
    [],
  )

  useEffect(() => {
    load('')
  }, [load])

  const reloadCurrent = () => load(stack[stack.length - 1])

  const onNext = () => {
    if (!nextCursor) return
    const newStack = [...stack, nextCursor]
    setStack(newStack)
    load(nextCursor)
  }

  const onPrev = () => {
    if (stack.length <= 1) return
    const newStack = stack.slice(0, -1)
    setStack(newStack)
    load(newStack[newStack.length - 1])
  }

  const confirmBan = async () => {
    if (!banTarget) return
    if (!banReason.trim()) {
      message.warning('封禁原因为必填（将记入审计）')
      return
    }
    setBanSubmitting(true)
    try {
      await banInstall(banTarget.id, banReason.trim())
      message.success(`已封禁 ${banTarget.id}`)
      setBanTarget(null)
      setBanReason('')
      reloadCurrent()
    } catch (e) {
      if (e instanceof ApiError) message.error(e.message)
    } finally {
      setBanSubmitting(false)
    }
  }

  const onUnban = async (row: InstallRow) => {
    try {
      await unbanInstall(row.id)
      message.success(`已解封 ${row.id}`)
      reloadCurrent()
    } catch (e) {
      if (e instanceof ApiError) message.error(e.message)
    }
  }

  const columns: ColumnsType<InstallRow> = [
    {
      title: 'Install ID',
      dataIndex: 'id',
      key: 'id',
      render: (id: string) => <Text code copyable={{ text: id }}>{id}</Text>,
    },
    {
      title: '状态',
      dataIndex: 'status',
      key: 'status',
      render: statusTag,
    },
    {
      title: '今日支出 (USD)',
      dataIndex: 'todaySpendMicroUsd',
      key: 'todaySpendMicroUsd',
      align: 'right',
      render: formatMicroUsd,
    },
    {
      title: '创建时间',
      dataIndex: 'createdAt',
      key: 'createdAt',
      render: (t: string) => new Date(t).toLocaleString(),
    },
    {
      title: '最近活跃',
      dataIndex: 'lastSeenAt',
      key: 'lastSeenAt',
      render: (t?: string) => (t ? new Date(t).toLocaleString() : <Text type="secondary">—</Text>),
    },
    {
      title: '操作',
      key: 'action',
      render: (_: unknown, row: InstallRow) =>
        row.status === 'banned' ? (
          <Popconfirm title="解封该 install？" okText="解封" cancelText="取消" onConfirm={() => onUnban(row)}>
            <Button size="small" icon={<CheckCircleOutlined />}>
              解封
            </Button>
          </Popconfirm>
        ) : (
          <Button
            size="small"
            danger
            icon={<StopOutlined />}
            onClick={() => {
              setBanTarget(row)
              setBanReason('')
            }}
          >
            封禁
          </Button>
        ),
    },
  ]

  return (
    <div>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 16 }}>
        <Title level={3} style={{ margin: 0 }}>
          Installs
        </Title>
        <Button icon={<ReloadOutlined />} size="small" onClick={reloadCurrent}>
          刷新
        </Button>
      </div>

      {error && <Alert type="error" showIcon message={error} style={{ marginBottom: 16 }} />}

      <Table<InstallRow>
        rowKey="id"
        columns={columns}
        dataSource={rows}
        loading={loading}
        pagination={false}
        size="middle"
      />

      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginTop: 16 }}>
        <Text type="secondary">第 {stack.length} 页 · 每页 {PAGE_SIZE}</Text>
        <Space>
          <Button onClick={onPrev} disabled={stack.length <= 1 || loading}>
            上一页
          </Button>
          <Button onClick={onNext} disabled={!nextCursor || loading}>
            下一页
          </Button>
        </Space>
      </div>

      <Modal
        title={`封禁 install`}
        open={banTarget !== null}
        onCancel={() => setBanTarget(null)}
        onOk={confirmBan}
        okText="确认封禁"
        cancelText="取消"
        okButtonProps={{ danger: true, loading: banSubmitting }}
      >
        <Space direction="vertical" style={{ width: '100%' }}>
          <Text>
            目标：<Text code>{banTarget?.id}</Text>
          </Text>
          <Text type="secondary">原因为必填，将记入审计日志（最长 256 字符）。</Text>
          <Input.TextArea
            value={banReason}
            onChange={(e) => setBanReason(e.target.value)}
            placeholder="封禁原因"
            maxLength={256}
            rows={3}
            showCount
          />
        </Space>
      </Modal>
    </div>
  )
}

// Audit page — GET /api/audit (keyset cursor pagination over the in-process audit
// ring). The cursor is a monotonic seq; nextCursor is present only when older
// events remain. We keep a cursor stack for forward/back paging (the ring is
// bounded ~512 entries so this is the recent admin-action view).
//
// 审计:keyset 游标分页(seq),维护游标栈支持前后翻页;环有界(~512),为近期管理动作视图。

import { useCallback, useEffect, useState } from 'react'
import { Table, Button, Tag, Typography, Space, Alert } from 'antd'
import { ReloadOutlined } from '@ant-design/icons'
import type { ColumnsType } from 'antd/es/table'
import type { AuditEvent } from '../lib/types'
import { getAudit, ApiError } from '../lib/api'

const { Title, Text } = Typography
const PAGE_SIZE = 50

function outcomeTag(outcome: string) {
  switch (outcome) {
    case 'ok':
      return <Tag color="green">ok</Tag>
    case 'not_found':
      return <Tag color="orange">not_found</Tag>
    case 'interrupted':
      return <Tag color="gold">interrupted</Tag>
    case 'error':
      return <Tag color="red">error</Tag>
    default:
      return <Tag>{outcome}</Tag>
  }
}

function actionTag(action: string) {
  const color =
    action === 'ban' ? 'red' : action === 'unban' ? 'green' : action === 'config_update' ? 'blue' : 'purple'
  return <Tag color={color}>{action}</Tag>
}

export default function Audit() {
  const [rows, setRows] = useState<AuditEvent[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [stack, setStack] = useState<string[]>([''])
  const [nextCursor, setNextCursor] = useState('')

  const load = useCallback(async (cursor: string) => {
    setLoading(true)
    setError(null)
    try {
      const res = await getAudit(cursor, PAGE_SIZE)
      setRows(res.events ?? [])
      setNextCursor(res.nextCursor ?? '')
    } catch (e) {
      if (e instanceof ApiError && e.status !== 401) setError(e.message)
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    load('')
  }, [load])

  const reloadCurrent = () => {
    // Reload resets to the newest page (a fresh keyset walk) since new events may
    // have landed at the head.
    setStack([''])
    load('')
  }

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

  const columns: ColumnsType<AuditEvent> = [
    { title: 'Seq', dataIndex: 'seq', key: 'seq', width: 80 },
    {
      title: '时间',
      dataIndex: 'at',
      key: 'at',
      render: (t: string) => new Date(t).toLocaleString(),
    },
    { title: '动作', dataIndex: 'action', key: 'action', render: actionTag },
    {
      title: '目标',
      dataIndex: 'target',
      key: 'target',
      render: (t: string) => (t ? <Text code>{t}</Text> : <Text type="secondary">—</Text>),
    },
    {
      title: '原因',
      dataIndex: 'reason',
      key: 'reason',
      render: (r: string) => (r ? <Text>{r}</Text> : <Text type="secondary">—</Text>),
    },
    { title: '结果', dataIndex: 'outcome', key: 'outcome', render: outcomeTag },
    {
      title: '操作者',
      dataIndex: 'actor',
      key: 'actor',
      render: (a: string) => (a ? <Text>{a}</Text> : <Text type="secondary">—</Text>),
    },
  ]

  return (
    <div>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 16 }}>
        <Title level={3} style={{ margin: 0 }}>
          审计
        </Title>
        <Button icon={<ReloadOutlined />} size="small" onClick={reloadCurrent}>
          刷新
        </Button>
      </div>

      <Alert
        type="info"
        showIcon
        style={{ marginBottom: 16 }}
        message="进程内审计环（最近 ~512 条）：后台自身的封禁/解封/配置变更/导出动作。只含安全字段，绝不含 token/key/IP。"
      />

      {error && <Alert type="error" showIcon message={error} style={{ marginBottom: 16 }} />}

      <Table<AuditEvent>
        rowKey="seq"
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
            上一页（较新）
          </Button>
          <Button onClick={onNext} disabled={!nextCursor || loading}>
            下一页（较旧）
          </Button>
        </Space>
      </div>
    </div>
  )
}

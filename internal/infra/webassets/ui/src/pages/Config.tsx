// Config page — GET /api/config renders the runtime config read model; POST
// /api/config applies a single {key:value} override (CSRF). Bounded numeric items
// carry min/max for client pre-validation (the SAME bounds the server enforces);
// a CONFIG_REJECTED is shown inline on the offending row. Non-editable items
// (restart-required) are read-only. Secrets never appear in the dump by
// construction (the backend omits them).
//
// 配置:可编辑项带 min/max 预校验 + restart 徽章;CONFIG_REJECTED 行内报错;
// 非编辑项只读。机密项后端从不下发(掩码即"不在 dump 里")。

import { useCallback, useEffect, useState } from 'react'
import { Table, Button, Input, InputNumber, Tag, Typography, Space, Alert, App } from 'antd'
import { ReloadOutlined, SaveOutlined } from '@ant-design/icons'
import type { ColumnsType } from 'antd/es/table'
import type { DumpItem } from '../lib/types'
import { getConfig, postConfig, ApiError } from '../lib/api'

const { Title, Text } = Typography

// isBoundedNumeric: a runtime item with both min & max is edited via InputNumber
// with the inclusive bounds the server also enforces.
function isBoundedNumeric(it: DumpItem): boolean {
  return it.editable && typeof it.min === 'number' && typeof it.max === 'number'
}

export default function Config() {
  const { message } = App.useApp()
  const [items, setItems] = useState<DumpItem[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)

  // Per-row draft value + inline rejection message, keyed by config key.
  const [drafts, setDrafts] = useState<Record<string, string>>({})
  const [rowError, setRowError] = useState<Record<string, string>>({})
  const [savingKey, setSavingKey] = useState<string | null>(null)

  const load = useCallback(async () => {
    setLoading(true)
    setError(null)
    try {
      const res = await getConfig()
      const list = res.items ?? []
      setItems(list)
      // Reset drafts to the live values on (re)load.
      const d: Record<string, string> = {}
      for (const it of list) d[it.key] = it.value
      setDrafts(d)
      setRowError({})
    } catch (e) {
      if (e instanceof ApiError && e.status !== 401) setError(e.message)
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    load()
  }, [load])

  const save = async (it: DumpItem) => {
    const val = drafts[it.key] ?? ''
    // Client-side bound pre-check mirrors the server CONFIG_REJECTED guard.
    if (isBoundedNumeric(it)) {
      const n = Number(val)
      if (!Number.isFinite(n) || !Number.isInteger(n)) {
        setRowError((e) => ({ ...e, [it.key]: '必须为整数' }))
        return
      }
      if (n < (it.min as number) || n > (it.max as number)) {
        setRowError((e) => ({ ...e, [it.key]: `超出范围 [${it.min}, ${it.max}]` }))
        return
      }
    }
    setSavingKey(it.key)
    setRowError((e) => ({ ...e, [it.key]: '' }))
    try {
      const res = await postConfig({ [it.key]: val })
      const list = res.items ?? []
      setItems(list)
      const d: Record<string, string> = {}
      for (const x of list) d[x.key] = x.value
      setDrafts(d)
      message.success(`已更新 ${it.key}`)
    } catch (e) {
      if (e instanceof ApiError) {
        if (e.code === 'CONFIG_REJECTED') {
          setRowError((er) => ({ ...er, [it.key]: e.message }))
        } else {
          message.error(e.message)
        }
      }
    } finally {
      setSavingKey(null)
    }
  }

  const columns: ColumnsType<DumpItem> = [
    {
      title: '配置项',
      dataIndex: 'key',
      key: 'key',
      width: '28%',
      render: (key: string, it: DumpItem) => (
        <Space direction="vertical" size={2}>
          <Text strong style={{ fontFamily: 'monospace' }}>
            {key}
          </Text>
          <Space size={4} wrap>
            {it.editable ? <Tag color="blue">可编辑</Tag> : <Tag>只读</Tag>}
            {it.restartRequired && <Tag color="orange">需重启生效</Tag>}
            {typeof it.min === 'number' && typeof it.max === 'number' && (
              <Tag>
                范围 [{it.min}, {it.max}]
              </Tag>
            )}
          </Space>
        </Space>
      ),
    },
    {
      title: '值',
      key: 'value',
      render: (_: unknown, it: DumpItem) => {
        if (!it.editable) {
          return (
            <Text type="secondary" style={{ fontFamily: 'monospace', wordBreak: 'break-all' }}>
              {it.value || <Text type="secondary">（空）</Text>}
            </Text>
          )
        }
        const err = rowError[it.key]
        return (
          <Space direction="vertical" style={{ width: '100%' }} size={4}>
            {isBoundedNumeric(it) ? (
              <InputNumber
                value={drafts[it.key] === '' ? null : Number(drafts[it.key])}
                min={it.min}
                max={it.max}
                step={1}
                style={{ width: 220 }}
                status={err ? 'error' : undefined}
                onChange={(v) => setDrafts((d) => ({ ...d, [it.key]: v == null ? '' : String(v) }))}
              />
            ) : (
              <Input
                value={drafts[it.key] ?? ''}
                style={{ maxWidth: 480 }}
                status={err ? 'error' : undefined}
                onChange={(e) => setDrafts((d) => ({ ...d, [it.key]: e.target.value }))}
              />
            )}
            {err && (
              <Text type="danger" style={{ fontSize: 12 }}>
                {err}
              </Text>
            )}
          </Space>
        )
      },
    },
    {
      title: '操作',
      key: 'action',
      width: 120,
      render: (_: unknown, it: DumpItem) => {
        if (!it.editable) return <Text type="secondary">—</Text>
        const dirty = (drafts[it.key] ?? '') !== it.value
        return (
          <Button
            type="primary"
            size="small"
            icon={<SaveOutlined />}
            disabled={!dirty}
            loading={savingKey === it.key}
            onClick={() => save(it)}
          >
            保存
          </Button>
        )
      },
    },
  ]

  return (
    <div>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 16 }}>
        <Title level={3} style={{ margin: 0 }}>
          配置
        </Title>
        <Button icon={<ReloadOutlined />} size="small" onClick={load}>
          刷新
        </Button>
      </div>

      <Alert
        type="info"
        showIcon
        style={{ marginBottom: 16 }}
        message="机密项（上游 API key / 后台口令）永不出现在此列表；标注「需重启生效」的项改后需重启网关进程方生效。"
      />

      {error && <Alert type="error" showIcon message={error} style={{ marginBottom: 16 }} />}

      <Table<DumpItem>
        rowKey="key"
        columns={columns}
        dataSource={items}
        loading={loading}
        pagination={false}
        size="middle"
      />
    </div>
  )
}

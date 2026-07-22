// Overview page — the live operational snapshot from GET /api/overview. Polls on
// a fixed interval but PAUSES while the tab is hidden (visibilitychange) so a
// backgrounded dashboard does not hammer the gateway; it refetches immediately on
// becoming visible again.
//
// 总览:定时轮询 /api/overview,页面隐藏时暂停(visibilitychange),复显即刷。

import { useCallback, useEffect, useRef, useState } from 'react'
import {
  Row,
  Col,
  Card,
  Statistic,
  Progress,
  Tag,
  Typography,
  Empty,
  Alert,
  Spin,
  Space,
  Button,
  App,
  Input,
  Modal,
} from 'antd'
import { ReloadOutlined } from '@ant-design/icons'
import type { OverviewResponse, AlertState, ProviderStatus } from '../lib/types'
import { getOverview, resetAllMonthlyQuota, ApiError } from '../lib/api'
import { formatMicroUsd } from '../lib/money'

const { Title, Text } = Typography
const REFRESH_MS = 10_000

function fmtInt(n: number): string {
  return n.toLocaleString('en-US')
}

function severityColor(sev: string): string {
  switch (sev) {
    case 'critical':
      return 'red'
    case 'warning':
      return 'orange'
    default:
      return 'blue'
  }
}

function ProviderCard({ name, route, state }: { name: string; route: string; state: ProviderStatus }) {
  return (
    <Card>
      <Space direction="vertical" size={8} style={{ width: '100%' }}>
        <div>
          <Text strong>{name}</Text>
          <Text type="secondary"> · {route}</Text>
        </div>
        <Space wrap>
          {state.configured ? <Tag color="green">已配置</Tag> : <Tag>未配置</Tag>}
          {!state.configured ? (
            <Tag>BREAKER N/A</Tag>
          ) : state.breakerOpen ? (
            <Tag color="red">BREAKER OPEN</Tag>
          ) : (
            <Tag color="green">BREAKER CLOSED</Tag>
          )}
        </Space>
      </Space>
    </Card>
  )
}

export default function Overview() {
  const { message } = App.useApp()
  const [data, setData] = useState<OverviewResponse | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [resettingQuota, setResettingQuota] = useState(false)
  const timer = useRef<number | null>(null)

  const refresh = useCallback(async () => {
    try {
      const d = await getOverview()
      setData(d)
      setError(null)
    } catch (e) {
      // A 401 is handled globally (redirect); only surface real errors here.
      if (e instanceof ApiError && e.status !== 401) {
        setError(e.message)
      }
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    let stopped = false

    const start = () => {
      if (timer.current !== null) return
      timer.current = window.setInterval(() => {
        if (!stopped) refresh()
      }, REFRESH_MS)
    }
    const stop = () => {
      if (timer.current !== null) {
        window.clearInterval(timer.current)
        timer.current = null
      }
    }
    const onVisibility = () => {
      if (document.hidden) {
        stop()
      } else {
        refresh()
        start()
      }
    }

    refresh()
    if (!document.hidden) start()
    document.addEventListener('visibilitychange', onVisibility)
    return () => {
      stopped = true
      stop()
      document.removeEventListener('visibilitychange', onVisibility)
    }
  }, [refresh])

  const confirmQuotaReset = () => {
    let reason = ''
    Modal.confirm({
      title: '重置全员本月请求额度？',
      content: (
        <Space direction="vertical" size={12} style={{ width: '100%', marginTop: 8 }}>
          <Text>
            此操作会清零当前月所有 install 的已用请求次数，让每个人重新获得其配置的月额度。全局支出预算和成本账本不会改变。
          </Text>
          <Input.TextArea
            autoFocus
            maxLength={256}
            rows={3}
            placeholder="重置原因（将记录到审计）"
            showCount
            onChange={(event) => {
              reason = event.target.value
            }}
          />
        </Space>
      ),
      okText: '确认重置',
      cancelText: '取消',
      okButtonProps: { danger: true },
      onOk: async () => {
        const trimmed = reason.trim()
        if (!trimmed) {
          message.warning('请填写重置原因')
          return Promise.reject(new Error('reset reason required'))
        }
        setResettingQuota(true)
        try {
          const result = await resetAllMonthlyQuota(trimmed)
          message.success(`已重置 ${result.period} 月额度（${fmtInt(result.resetInstalls)} 个 install 有已用次数）`)
          await refresh()
        } catch (e) {
          if (e instanceof ApiError) {
            message.error(e.message)
          } else {
            message.error('重置额度失败')
          }
          throw e
        } finally {
          setResettingQuota(false)
        }
      },
    })
  }

  if (loading && !data) {
    return (
      <div style={{ textAlign: 'center', padding: 80 }}>
        <Spin size="large" />
      </div>
    )
  }

  const budgetPct =
    data && data.budget.limitMicroUsd > 0
      ? Math.min(100, (data.budget.usedMicroUsd / data.budget.limitMicroUsd) * 100)
      : 0
  const alerts: AlertState[] = data?.alerts ?? []
  const firing = alerts.filter((a) => a.firing)

  return (
    <div>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 16 }}>
        <Title level={3} style={{ margin: 0 }}>
          总览
        </Title>
        <Space>
          <Text type="secondary">每 {REFRESH_MS / 1000}s 自动刷新</Text>
          <Button icon={<ReloadOutlined />} size="small" onClick={refresh}>
            刷新
          </Button>
        </Space>
      </div>

      {error && <Alert type="error" showIcon message={error} style={{ marginBottom: 16 }} />}

      {data && (
        <>
          <Card title="本月全局支出预算" style={{ marginBottom: 16 }} extra={<Text type="secondary">{data.budget.day}</Text>}>
            <Row gutter={[24, 16]}>
              <Col xs={24} sm={8}>
                <Statistic title="已用 (USD)" value={formatMicroUsd(data.budget.usedMicroUsd)} />
              </Col>
              <Col xs={24} sm={8}>
                <Statistic title="剩余 (USD)" value={formatMicroUsd(data.budget.remainingMicroUsd)} />
              </Col>
              <Col xs={24} sm={8}>
                <Statistic title="上限 (USD)" value={formatMicroUsd(data.budget.limitMicroUsd)} />
              </Col>
            </Row>
            <Progress
              percent={Number(budgetPct.toFixed(1))}
              status={budgetPct >= 95 ? 'exception' : budgetPct >= 80 ? 'active' : 'normal'}
              style={{ marginTop: 16 }}
            />
          </Card>

          <Card
            title="全员月请求额度"
            style={{ marginBottom: 16 }}
            extra={
              <Button
                danger
                onClick={confirmQuotaReset}
                loading={resettingQuota}
                disabled={data.openReservations > 0}
              >
                重置全员额度
              </Button>
            }
          >
            <Space direction="vertical" size={4}>
              <Text>将当前月所有 install 的已用请求次数清零；每人重新获得其配置的月请求额度。</Text>
              <Text type="secondary">不会重置全局支出预算，也不会删除成本账本或历史消费。</Text>
              {data.openReservations > 0 && (
                <Text type="warning">当前有 {fmtInt(data.openReservations)} 个请求待结算；完成后才能安全重置。</Text>
              )}
            </Space>
          </Card>

          <Row gutter={[16, 16]} style={{ marginBottom: 16 }}>
            <Col xs={12} sm={8} md={6}>
              <Card>
                <Statistic title="并发处理中 (inflight)" value={data.inflightConcurrency} />
              </Card>
            </Col>
            <Col xs={12} sm={8} md={6}>
              <Card>
                <Statistic title="待结算预占 (open reservations)" value={data.openReservations} />
              </Card>
            </Col>
            <Col xs={12} sm={8} md={6}>
              <Card>
                <Statistic
                  title="今日新增 install"
                  value={data.installsToday}
                  suffix={data.installGlobalCap > 0 ? `/ ${fmtInt(data.installGlobalCap)}` : '/ 无上限'}
                />
              </Card>
            </Col>
            <Col xs={12} sm={8} md={6}>
              <Card>
                <Statistic
                  title="近窗 QPS"
                  value={data.recent.qps.toFixed(2)}
                  suffix={data.recent.windowSec > 0 ? `(${data.recent.windowSec.toFixed(0)}s)` : ''}
                />
              </Card>
            </Col>
          </Row>

          <Row gutter={[16, 16]} style={{ marginBottom: 16 }}>
            <Col xs={24} md={8}>
              <ProviderCard name="DeepSeek" route="纯文本" state={data.providers.deepseek} />
            </Col>
            <Col xs={24} md={8}>
              <ProviderCard name="Kimi" route="图片 / 视频" state={data.providers.kimi} />
            </Col>
            <Col xs={24} md={8}>
              <Card>
                <Space direction="vertical" size={8} style={{ width: '100%' }}>
                  <Text strong>磁盘状态</Text>
                  {data.diskDegraded ? <Tag color="red">DEGRADED（只读降级）</Tag> : <Tag color="green">OK</Tag>}
                </Space>
              </Card>
            </Col>
          </Row>

          <Card title={`告警（${firing.length} 触发中 / ${alerts.length} 已配置）`}>
            {alerts.length === 0 ? (
              <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="无告警状态" />
            ) : (
              <Space direction="vertical" style={{ width: '100%' }}>
                {alerts.map((a) => (
                  <div
                    key={a.reason}
                    style={{
                      display: 'flex',
                      alignItems: 'center',
                      gap: 12,
                      padding: '8px 12px',
                      borderRadius: 6,
                      background: a.firing ? '#fff1f0' : '#fafafa',
                    }}
                  >
                    {a.firing ? (
                      <Tag color={severityColor(a.severity)}>触发中</Tag>
                    ) : (
                      <Tag>正常</Tag>
                    )}
                    <Text strong>{a.reason}</Text>
                    <Text type="secondary" style={{ flex: 1 }}>
                      {a.message}
                    </Text>
                    {a.lastFiredAt && (
                      <Text type="secondary" style={{ fontSize: 12 }}>
                        最近触发 {new Date(a.lastFiredAt).toLocaleString()}
                      </Text>
                    )}
                  </div>
                ))}
              </Space>
            )}
          </Card>
        </>
      )}
    </div>
  )
}

import { useEffect, useRef, useState } from 'react'
import { Link, useSearchParams } from 'react-router-dom'
import { request, ApiError } from '../api/client'
import type { AccountSummary, AliasListResult, Alias, FullMessage, InboxResult, InboxMessage } from '../api/types'
import AsyncState from '../components/AsyncState'
import Dialog from '../components/Dialog'
import ConfirmDialog from '../components/ConfirmDialog'
import { useToast } from '../components/ToastProvider'
import { IconKey, IconMail, IconTrash } from '../components/icons'

function formatDate(raw: string): string {
  const d = new Date(raw)
  if (Number.isNaN(d.getTime())) return raw
  return new Intl.DateTimeFormat('zh-CN', {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
  }).format(d)
}

export default function InboxPage() {
  const [accounts, setAccounts] = useState<AccountSummary[]>([])
  const [aliases, setAliases] = useState<Alias[]>([])
  const [accountId, setAccountId] = useState('')
  const [alias, setAlias] = useState('')
  const [limit, setLimit] = useState(20)
  const [days, setDays] = useState(7)

  const [result, setResult] = useState<InboxResult | null>(null)
  const [loading, setLoading] = useState(true)
  const [settledKey, setSettledKey] = useState('')
  const [error, setError] = useState('')
  const [retryKey, setRetryKey] = useState(0)
  const [detail, setDetail] = useState<FullMessage | null>(null)
  const [detailLoading, setDetailLoading] = useState(false)
  const [deleteFor, setDeleteFor] = useState<InboxMessage | null>(null)
  const [deleting, setDeleting] = useState(false)
  const [elapsedMs, setElapsedMs] = useState<number | null>(null)

  const [searchParams, setSearchParams] = useSearchParams()
  const abortRef = useRef<AbortController | null>(null)
  const detailAbortRef = useRef<AbortController | null>(null)
  const { show } = useToast()

  async function openMessage(message: InboxMessage) {
    detailAbortRef.current?.abort()
    const controller = new AbortController()
    detailAbortRef.current = controller
    setDetail(null)
    setDetailLoading(true)
    try {
      const data = await request<FullMessage>(
        `/api/inbox/${encodeURIComponent(message.id)}?account_id=${encodeURIComponent(accountId)}`,
        { signal: controller.signal },
      )
      if (!controller.signal.aborted) setDetail(data)
    } catch (err) {
      if (err instanceof ApiError && err.status === 0) return
      show(err instanceof ApiError ? err.message : '读取邮件详情失败')
    } finally {
      if (!controller.signal.aborted) setDetailLoading(false)
    }
  }

  function closeDetail() {
    detailAbortRef.current?.abort()
    setDetail(null)
    setDetailLoading(false)
  }

  async function deleteMessage() {
    if (!deleteFor) return
    const target = deleteFor
    setDeleting(true)
    try {
      await request(`/api/inbox/${encodeURIComponent(target.id)}?account_id=${encodeURIComponent(accountId)}`, { method: 'DELETE' })
      setDeleteFor(null)
      if (detail?.id === target.id) closeDetail()
      // 本地移除,不再整页重新拉取收件箱
      setResult((prev) =>
        prev
          ? { ...prev, messages: prev.messages.filter((m) => m.id !== target.id), count: Math.max(0, prev.count - 1) }
          : prev,
      )
      show('邮件已删除')
    } catch (err) {
      show(err instanceof ApiError ? err.message : '删除邮件失败')
    } finally {
      setDeleting(false)
    }
  }

  // 加载账号列表并初始化筛选状态(只保存 account_id/alias/limit/days)
  useEffect(() => {
    let cancelled = false
    request<AccountSummary[]>('/api/accounts')
      .then((data) => {
        if (cancelled) return
        setAccounts(data)
        const queryId = searchParams.get('account_id')
        const valid = data.find((a) => a.id === queryId)
        const target = valid ? valid.id : data[0]?.id ?? ''
        setAccountId(target)
        if (!target) setLoading(false)
        if (target) {
          const next: Record<string, string> = { account_id: target }
          const qAlias = searchParams.get('alias')
          if (qAlias) {
            setAlias(qAlias)
            next.alias = qAlias
          }
          const qLimit = searchParams.get('limit')
          if (qLimit && Number(qLimit) > 0) {
            setLimit(Number(qLimit))
            next.limit = qLimit
          }
          const qDays = searchParams.get('days')
          if (qDays && Number(qDays) > 0) {
            setDays(Number(qDays))
            next.days = qDays
          }
          setSearchParams(next, { replace: true })
        }
      })
      .catch((err) => {
        if (cancelled) return
        setError(err instanceof ApiError ? err.message : '网络连接失败，请检查服务状态')
        setLoading(false)
      })
    return () => {
      cancelled = true
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // 账号变化时加载别名列表(供筛选;后端有缓存,失败不影响收件箱)
  useEffect(() => {
    if (!accountId) return
    let cancelled = false
    request<AliasListResult>(`/api/aliases?account_id=${encodeURIComponent(accountId)}`)
      .then((data) => {
        if (!cancelled) setAliases(data.aliases ?? [])
      })
      .catch(() => {
        if (!cancelled) setAliases([])
      })
    return () => {
      cancelled = true
    }
  }, [accountId])

  // 查询收件箱;条件变化时中止旧请求,已有结果保留到新结果返回
  useEffect(() => {
    if (!accountId) return
    abortRef.current?.abort()
    const controller = new AbortController()
    abortRef.current = controller
    let cancelled = false
    const params = new URLSearchParams({ account_id: accountId })
    if (alias) params.set('alias', alias)
    params.set('limit', String(limit))
    params.set('days', String(days))
    const key = `${accountId}|${alias}|${limit}|${days}|${retryKey}`
    const started = performance.now()
    request<InboxResult>(`/api/inbox?${params.toString()}`, {
      signal: controller.signal,
      timeoutMs: 60_000,
    })
      .then((data) => {
        if (cancelled) return
        setResult(data)
        setError('')
        setElapsedMs(Math.round(performance.now() - started))
      })
      .catch((err) => {
        if (cancelled || (err instanceof ApiError && err.status === 0)) return
        setError(err instanceof ApiError ? err.message : '网络连接失败，请检查服务状态')
        setResult(null)
      })
      .finally(() => {
        if (!cancelled) {
          setLoading(false)
          setSettledKey(key)
        }
      })
    return () => {
      cancelled = true
      controller.abort()
    }
  }, [accountId, alias, limit, days, retryKey])

  function syncParams(next: { alias?: string; limit?: number; days?: number }) {
    const params: Record<string, string> = { account_id: accountId }
    const a = next.alias ?? alias
    if (a) params.alias = a
    params.limit = String(next.limit ?? limit)
    params.days = String(next.days ?? days)
    setSearchParams(params, { replace: true })
  }

  function handleSearch() {
    syncParams({})
    setRetryKey((k) => k + 1)
  }

  function handleAccountChange(id: string) {
    setAccountId(id)
    setAlias('')
    setResult(null)
    setLoading(true)
    setSearchParams({ account_id: id }, { replace: true })
  }

  const refreshing = Boolean(accountId) && settledKey !== `${accountId}|${alias}|${limit}|${days}|${retryKey}`
  const methodText = result?.method === 'imap' ? 'IMAP' : 'Web API'

  return (
    <section>
      <div className="page-header">
        <div className="page-title">
          <h2>收件箱摘要</h2>
          <p>查看发往隐私别名的邮件，点击主题查看完整正文</p>
        </div>
      </div>

      <div className="card" style={{ marginBottom: 16 }}>
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(160px, 1fr))', gap: 12 }}>
          <div className="form-field" style={{ marginBottom: 0 }}>
            <label htmlFor="inbox-account">账号</label>
            <select id="inbox-account" value={accountId} onChange={(e) => handleAccountChange(e.target.value)}>
              {accounts.map((a) => (
                <option key={a.id} value={a.id}>
                  {a.name}
                </option>
              ))}
            </select>
          </div>
          <div className="form-field" style={{ marginBottom: 0 }}>
            <label htmlFor="inbox-alias">别名</label>
            <select
              id="inbox-alias"
              value={alias}
              onChange={(e) => {
                setAlias(e.target.value)
                syncParams({ alias: e.target.value })
              }}
            >
              <option value="">全部</option>
              {alias && !aliases.some((a) => a.email === alias) && <option value={alias}>{alias}</option>}
              {aliases.map((a) => (
                <option key={a.anonymousId} value={a.email}>
                  {a.label ? `${a.email}（${a.label}）` : a.email}
                </option>
              ))}
            </select>
          </div>
          <div className="form-field" style={{ marginBottom: 0 }}>
            <label htmlFor="inbox-limit">每页</label>
            <select
              id="inbox-limit"
              value={limit}
              onChange={(e) => {
                setLimit(Number(e.target.value))
                syncParams({ limit: Number(e.target.value) })
              }}
            >
              <option value={1}>1</option>
              <option value={20}>20</option>
              <option value={50}>50</option>
              <option value={100}>100</option>
            </select>
          </div>
          <div className="form-field" style={{ marginBottom: 0 }}>
            <label htmlFor="inbox-days">时间范围</label>
            <select
              id="inbox-days"
              value={days}
              onChange={(e) => {
                setDays(Number(e.target.value))
                syncParams({ days: Number(e.target.value) })
              }}
            >
              <option value={1}>1 天</option>
              <option value={7}>7 天</option>
              <option value={30}>30 天</option>
              <option value={90}>90 天</option>
            </select>
          </div>
          <div style={{ display: 'flex', alignItems: 'flex-end' }}>
            <button className="primary" onClick={handleSearch} disabled={refreshing && !loading}>
              {refreshing && !loading ? '查询中…' : '查询'}
            </button>
          </div>
        </div>
      </div>

      <AsyncState
        loading={loading}
        error={error}
        empty={!result || result.messages.length === 0}
        emptyText="暂无邮件"
        onRetry={() => {
          setLoading(true)
          setRetryKey((k) => k + 1)
        }}
      >
        {result && result.messages.length > 0 && (
          <>
            <p className="hint list-meta">
              <span>共 {result.messages.length} 封</span>
              <span className={result.method === 'imap' ? 'badge badge-info' : 'badge badge-neutral'}>
                {result.method === 'imap' ? <IconKey size={12} /> : <IconMail size={12} />}
                读取方式：{methodText}
              </span>
              {elapsedMs !== null && <span>耗时 {(elapsedMs / 1000).toFixed(1)} 秒</span>}
              {refreshing && <span>正在刷新…</span>}
            </p>
            <div className="table-wrap">
              <table className="inbox-table">
                <colgroup>
                  <col className="col-subject" />
                  <col className="col-from" />
                  <col className="col-to" />
                  <col className="col-date" />
                  <col className="col-preview" />
                  <col className="col-actions" />
                </colgroup>
                <thead>
                  <tr>
                    <th>主题</th>
                    <th>发件人</th>
                    <th>收件人</th>
                    <th>日期</th>
                    <th>摘要</th>
                    <th className="inbox-actions">操作</th>
                  </tr>
                </thead>
                <tbody>
                  {result.messages.map((m) => (
                    <tr key={m.id}>
                      <td>
                        <button className="link-button cell-clamp cell-clamp-strong" title={m.subject} onClick={() => void openMessage(m)}>
                          {m.subject || '（无主题）'}
                        </button>
                      </td>
                      <td>
                        <span className="cell-ellipsis" title={m.from}>{m.from || '—'}</span>
                      </td>
                      <td>
                        <span className="cell-ellipsis" title={m.to}>{m.to || '—'}</span>
                      </td>
                      <td className="cell-nowrap">{formatDate(m.date)}</td>
                      <td>
                        <span className="cell-clamp" title={m.preview}>{m.preview || '—'}</span>
                      </td>
                      <td className="inbox-actions">
                        <button
                          className="icon-button danger"
                          aria-label="删除邮件"
                          title="删除邮件"
                          disabled={deleting && deleteFor?.id === m.id}
                          onClick={() => setDeleteFor(m)}
                        >
                          <IconTrash size={16} />
                        </button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </>
        )}
      </AsyncState>

      {!loading && !error && accounts.length === 0 && (
        <p className="hint">
          暂无账号，请先到 <Link to="/accounts">账号</Link> 页面添加。
        </p>
      )}

      <Dialog title={detail?.subject || '邮件详情'} open={detail !== null || detailLoading} onClose={closeDetail}>
        {detailLoading && <p className="hint">读取中…</p>}
        {detail && (
          <>
            <p className="hint">发件人：{detail.from}</p>
            <p className="hint">收件人：{detail.to}</p>
            <p className="hint">日期：{formatDate(detail.date)}</p>
            <pre className="mail-body">{detail.body || '无正文'}</pre>
            <div className="form-actions">
              <button className="danger" onClick={() => setDeleteFor(detail)}>
                删除邮件
              </button>
              <button onClick={closeDetail}>关闭</button>
            </div>
          </>
        )}
      </Dialog>
      {deleteFor && (
        <ConfirmDialog
          title="删除邮件"
          message={`「${deleteFor.subject || '（无主题）'}」将从收件箱中永久删除。`}
          open
          busy={deleting}
          onClose={() => setDeleteFor(null)}
          onConfirm={() => void deleteMessage()}
        />
      )}
    </section>
  )
}

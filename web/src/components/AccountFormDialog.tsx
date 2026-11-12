import { useState } from 'react'
import Dialog from './Dialog'
import { request, ApiError } from '../api/client'

interface AccountFormDialogProps {
  open: boolean
  onClose: () => void
  onSaved: () => void
  editing?: {
    id: string
    name: string
    icloudEmail: string
    host: string
  } | null
}

/**
 * 添加账号 / 编辑基本信息对话框。
 *
 * 父组件按需挂载(并以账号 ID 作为 key),表单初始值直接取自 editing,
 * 修复旧版常驻挂载导致「编辑」时表单为空、邮箱被禁用且为空而无法保存的问题。
 */
export default function AccountFormDialog({ open, onClose, onSaved, editing }: AccountFormDialogProps) {
  const [name, setName] = useState(editing?.name ?? '')
  const [icloudEmail, setIcloudEmail] = useState(editing?.icloudEmail ?? '')
  const [host, setHost] = useState(editing?.host || 'icloud.com')
  const [cookies, setCookies] = useState('')
  const [proxy, setProxy] = useState('')
  const [error, setError] = useState('')
  const [submitting, setSubmitting] = useState(false)

  const changes: Record<string, string> = {}
  if (editing) {
    if (name.trim() !== editing.name) changes.name = name.trim()
    if (icloudEmail.trim() !== (editing.icloudEmail ?? '')) changes.icloud_email = icloudEmail.trim()
    if (host !== (editing.host || 'icloud.com')) changes.host = host
  }
  const unchanged = Boolean(editing) && Object.keys(changes).length === 0

  async function handleSubmit() {
    if (submitting || unchanged) return
    if (!name.trim()) {
      setError('请输入账号名称')
      return
    }
    if (!icloudEmail.trim()) {
      setError('请输入邮箱')
      return
    }
    setSubmitting(true)
    setError('')
    try {
      if (editing) {
        // 只提交修改过的字段
        await request(`/api/accounts/${encodeURIComponent(editing.id)}`, {
          method: 'PATCH',
          body: JSON.stringify(changes),
        })
      } else {
        await request('/api/accounts', {
          method: 'POST',
          body: JSON.stringify({ name: name.trim(), icloud_email: icloudEmail.trim(), host, proxy, cookies }),
          timeoutMs: 60_000,
        })
      }
      onSaved()
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '网络连接失败，请检查服务状态')
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Dialog title={editing ? '编辑账号' : '添加账号'} open={open} onClose={onClose}>
      {error && (
        <div className="alert-error" role="alert">
          {error}
        </div>
      )}
      <form
        onSubmit={(e) => {
          e.preventDefault()
          void handleSubmit()
        }}
      >
        <div className="form-field">
          <label htmlFor="acc-name">名称</label>
          <input id="acc-name" value={name} onChange={(e) => setName(e.target.value)} maxLength={64} />
        </div>
        <div className="form-field">
          <label htmlFor="acc-email">邮箱（Apple ID 或 iCloud 邮箱）</label>
          <input id="acc-email" type="email" value={icloudEmail} onChange={(e) => setIcloudEmail(e.target.value)} />
          {editing && <p className="hint">用于「iCloud 登录」和 App 专用密码收信；Cookie 与已接入的收件邮箱不受影响。</p>}
        </div>
        <div className="form-field">
          <label htmlFor="acc-host">区域</label>
          <select id="acc-host" value={host} onChange={(e) => setHost(e.target.value)}>
            <option value="icloud.com">全球区 (icloud.com)</option>
            <option value="icloud.com.cn">中国区 (icloud.com.cn)</option>
          </select>
          {editing && host !== (editing.host || 'icloud.com') && (
            <p className="hint">修改区域后会重新建立 iCloud 会话，请确认 Cookie 属于该区域。</p>
          )}
        </div>
        {!editing && (
          <>
            <div className="form-field">
              <label htmlFor="acc-cookies">Cookie（可选，原始文本）</label>
              <textarea
                id="acc-cookies"
                value={cookies}
                onChange={(e) => setCookies(e.target.value)}
                spellCheck={false}
                placeholder="a=1; b=2"
              />
              <p className="hint">可粘贴 Cookie Header 字符串或 JSON。</p>
            </div>
            <div className="form-field">
              <label htmlFor="acc-proxy">代理（可选）</label>
              <input
                id="acc-proxy"
                type="text"
                value={proxy}
                onChange={(e) => setProxy(e.target.value)}
                placeholder="http://user:pass@host:port"
              />
            </div>
          </>
        )}
        <div className="form-actions">
          <button type="button" onClick={onClose}>
            取消
          </button>
          <button type="submit" className="primary" disabled={submitting || unchanged}>
            {submitting ? '保存中…' : '保存'}
          </button>
        </div>
      </form>
    </Dialog>
  )
}

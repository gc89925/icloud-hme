import { useState } from 'react'
import Dialog from './Dialog'
import { request, ApiError } from '../api/client'
import type { Alias } from '../api/types'

interface EditLabelDialogProps {
  accountId: string
  alias: Alias
  onClose: () => void
  onSaved: (alias: Alias) => void
}

/** 修改别名标签。点击遮罩不会关闭,输入内容不会因误点丢失。 */
export default function EditLabelDialog({ accountId, alias, onClose, onSaved }: EditLabelDialogProps) {
  const [label, setLabel] = useState(alias.label)
  const [error, setError] = useState('')
  const [submitting, setSubmitting] = useState(false)

  const trimmed = label.trim()
  const unchanged = trimmed === alias.label.trim()

  async function handleSubmit() {
    if (submitting || unchanged) return
    if (!trimmed) {
      setError('请输入标签')
      return
    }
    setSubmitting(true)
    setError('')
    try {
      await request<Alias>(`/api/aliases/${encodeURIComponent(alias.anonymousId)}`, {
        method: 'PATCH',
        // 带上原备注,避免 iCloud 接口把备注覆盖为空
        body: JSON.stringify({ account_id: accountId, label: trimmed, note: alias.note ?? '' }),
      })
      onSaved({ ...alias, label: trimmed })
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '网络连接失败，请检查服务状态')
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Dialog title="修改标签" open onClose={onClose}>
      {error && (
        <div className="alert-error" role="alert">
          {error}
        </div>
      )}
      <p className="hint" style={{ marginBottom: 12 }}>
        邮箱：<span className="cell-strong">{alias.email}</span>
      </p>
      <form
        onSubmit={(e) => {
          e.preventDefault()
          void handleSubmit()
        }}
      >
        <div className="form-field">
          <label htmlFor="edit-alias-label">新标签</label>
          <input
            id="edit-alias-label"
            value={label}
            onChange={(e) => setLabel(e.target.value.slice(0, 200))}
            maxLength={200}
            placeholder="例如：ChatGPT 普通号"
          />
          <p className="hint">只修改标签，邮箱地址和邮件转发不受影响；最长 200 字符。</p>
        </div>
        <div className="form-actions">
          <button type="button" onClick={onClose}>
            取消
          </button>
          <button type="submit" className="primary" disabled={submitting || unchanged || !trimmed}>
            {submitting ? '保存中…' : '保存'}
          </button>
        </div>
      </form>
    </Dialog>
  )
}

import { useState } from 'react'
import Dialog from './Dialog'

interface ConfirmDialogProps {
  title: string
  message: string
  confirmLabel?: string
  requireText?: string
  requireLabel?: string
  open: boolean
  onClose: () => void
  onConfirm: () => void
  busy?: boolean
}

/** 破坏性操作确认对话框:可要求输入精确匹配文本后才可确认 */
export default function ConfirmDialog({
  title,
  message,
  confirmLabel = '确认删除',
  requireText,
  requireLabel = '输入账号名称',
  open,
  onClose,
  onConfirm,
  busy = false,
}: ConfirmDialogProps) {
  const [input, setInput] = useState('')
  const matched = !requireText || input === requireText

  return (
    <Dialog
      title={title}
      open={open}
      onClose={() => {
        setInput('')
        onClose()
      }}
    >
      <p>{message}</p>
      {requireText && (
        <div className="form-field">
          <label htmlFor="confirm-text">{requireLabel}</label>
          <input
            id="confirm-text"
            value={input}
            onChange={(e) => setInput(e.target.value)}
            autoComplete="off"
          />
        </div>
      )}
      <div className="form-actions">
        <button
          onClick={() => {
            setInput('')
            onClose()
          }}
        >
          取消
        </button>
        <button
          className="danger"
          disabled={!matched || busy}
          onClick={() => {
            // 不在这里清空输入:操作失败时弹窗保持打开,用户无需重新输入确认文本
            onConfirm()
          }}
        >
          {busy ? '处理中…' : confirmLabel}
        </button>
      </div>
    </Dialog>
  )
}

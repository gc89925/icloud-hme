import { useEffect, useRef, type ReactNode } from 'react'

interface DialogProps {
  title: string
  open: boolean
  onClose: () => void
  children: ReactNode
}

/** 可访问 Dialog:Escape 关闭、焦点圈定、关闭后回到触发按钮 */
export default function Dialog({ title, open, onClose, children }: DialogProps) {
  const ref = useRef<HTMLDivElement>(null)
  const lastFocused = useRef<Element | null>(null)
  // 父组件通常传入内联箭头函数,每次渲染都是新引用。
  // 用 ref 保存最新的 onClose,让下面的 effect 只在打开/关闭时执行一次;
  // 否则每输入一个字符都会重新执行 effect,把焦点抢回第一个输入框,并打断中文输入法组字。
  const onCloseRef = useRef(onClose)
  useEffect(() => {
    onCloseRef.current = onClose
  }, [onClose])

  useEffect(() => {
    if (!open) return
    lastFocused.current = document.activeElement
    const node = ref.current
    if (!node) return
    const focusables = () =>
      Array.from(
        node.querySelectorAll<HTMLElement>(
          'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])',
        ),
      ).filter((el) => !el.hasAttribute('disabled'))
    // 打开时优先聚焦第一个表单字段,没有表单字段再聚焦第一个按钮
    const firstField = node.querySelector<HTMLElement>('input:not([disabled]), select:not([disabled]), textarea:not([disabled])')
    ;(firstField ?? focusables()[0])?.focus()

    const handleKey = (e: KeyboardEvent) => {
      // 输入法组字中按 Esc 是取消候选词,不应关闭弹窗
      if (e.isComposing || e.keyCode === 229) return
      if (e.key === 'Escape') {
        onCloseRef.current()
        return
      }
      if (e.key !== 'Tab') return
      const items = focusables()
      if (items.length === 0) return
      const first = items[0]
      const last = items[items.length - 1]
      if (e.shiftKey && document.activeElement === first) {
        e.preventDefault()
        last.focus()
      } else if (!e.shiftKey && document.activeElement === last) {
        e.preventDefault()
        first.focus()
      }
    }
    document.addEventListener('keydown', handleKey)
    return () => {
      document.removeEventListener('keydown', handleKey)
      if (lastFocused.current instanceof HTMLElement) {
        lastFocused.current.focus()
      }
    }
  }, [open])

  if (!open) return null

  // 点击遮罩不关闭:避免误点背景导致表单内容丢失。关闭请用按钮或 Esc。
  return (
    <div className="dialog-backdrop">
      <div
        ref={ref}
        className="dialog"
        role="dialog"
        aria-modal="true"
        aria-label={title}
      >
        <h3>{title}</h3>
        {children}
      </div>
    </div>
  )
}

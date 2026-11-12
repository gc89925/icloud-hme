import { useState } from 'react'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import Dialog from './Dialog'

function Form() {
  const [name, setName] = useState('')
  const [email, setEmail] = useState('')
  return (
    // 内联 onClose:每次渲染都是新函数,复现「输入时焦点跳回第一栏」的旧 bug
    <Dialog title="添加账号" open onClose={() => {}}>
      <label htmlFor="n">名称</label>
      <input id="n" value={name} onChange={(e) => setName(e.target.value)} />
      <label htmlFor="e">邮箱</label>
      <input id="e" value={email} onChange={(e) => setEmail(e.target.value)} />
      <button type="button">取消</button>
    </Dialog>
  )
}

describe('Dialog', () => {
  it('打开时聚焦第一个输入框,之后输入不会把焦点抢回第一栏', async () => {
    render(<Form />)
    const name = screen.getByLabelText('名称')
    const email = screen.getByLabelText('邮箱')
    expect(document.activeElement).toBe(name)
    const user = userEvent.setup()
    await user.type(name, '主号')
    await user.click(email)
    await user.type(email, 'abc@icloud.com')
    expect(document.activeElement).toBe(email)
    expect((email as HTMLInputElement).value).toBe('abc@icloud.com')
    expect((name as HTMLInputElement).value).toBe('主号')
  })

  it('点击遮罩不关闭', async () => {
    let closed = false
    render(
      <Dialog title="t" open onClose={() => (closed = true)}>
        <input aria-label="x" />
      </Dialog>,
    )
    await userEvent.setup().click(document.querySelector('.dialog-backdrop') as HTMLElement)
    expect(closed).toBe(false)
  })
})

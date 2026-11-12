import type { ApiResponse } from './types'

/** CSRF token,仅存 React 内存状态 */
let csrfToken: string | null = null

/** 全局 401 回调(由 AuthProvider 注册,避免循环 import) */
let unauthorizedHandler: (() => void) | null = null

/** 注册全局 401 回调 */
export function registerUnauthorizedHandler(handler: (() => void) | null): void {
  unauthorizedHandler = handler
}

/** 设置内存 CSRF token(由 AuthProvider 管理) */
export function setCSRFToken(token: string | null): void {
  csrfToken = token
}

/** ApiError 携带 HTTP 状态与稳定错误码 */
export class ApiError extends Error {
  readonly status: number
  readonly code: string

  constructor(status: number, code: string, message: string) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
  }
}

interface RequestOptions extends Omit<RequestInit, 'body'> {
  body?: unknown
  signal?: AbortSignal
  /** 超时毫秒数,默认 45 秒;0 表示不限制 */
  timeoutMs?: number
}

const DEFAULT_TIMEOUT_MS = 45_000

/**
 * 唯一的 fetch 入口。
 *
 * 统一设置 Accept、JSON Content-Type 与 credentials: same-origin;
 * 非 GET/HEAD/OPTIONS 自动携带 X-CSRF-Token;401 触发 onUnauthorized 回调。
 * 请求超过 timeoutMs 未返回时抛出 TIMEOUT 错误,避免页面无限转圈。
 */
export async function request<T>(
  path: string,
  init?: RequestOptions,
  onUnauthorized?: () => void,
): Promise<T> {
  const headers = new Headers(init?.headers)
  headers.set('Accept', 'application/json')
  headers.set('Content-Type', 'application/json')

  const method = (init?.method ?? 'GET').toUpperCase()
  if (method !== 'GET' && method !== 'HEAD' && method !== 'OPTIONS' && csrfToken) {
    headers.set('X-CSRF-Token', csrfToken)
  }

  let body: BodyInit | undefined
  if (init?.body !== undefined) {
    body = typeof init.body === 'string' ? init.body : JSON.stringify(init.body)
  }

  const timeoutMs = init?.timeoutMs ?? DEFAULT_TIMEOUT_MS
  const controller = new AbortController()
  let timedOut = false
  const outer = init?.signal
  const onOuterAbort = () => controller.abort()
  if (outer) {
    if (outer.aborted) controller.abort()
    else outer.addEventListener('abort', onOuterAbort, { once: true })
  }
  const timer =
    timeoutMs > 0
      ? setTimeout(() => {
          timedOut = true
          controller.abort()
        }, timeoutMs)
      : undefined

  const { timeoutMs: _ignored, signal: _signal, ...rest } = init ?? {}
  void _ignored
  void _signal

  let resp: Response
  try {
    resp = await fetch(path, {
      ...rest,
      method,
      headers,
      body,
      credentials: 'same-origin',
      signal: controller.signal,
    })
  } catch {
    if (timedOut) {
      throw new ApiError(408, 'TIMEOUT', `请求超时（${Math.round(timeoutMs / 1000)} 秒未响应），iCloud 或邮箱服务器响应过慢，请稍后重试`)
    }
    throw new ApiError(0, 'NETWORK_ERROR', '网络连接失败，请检查服务状态')
  } finally {
    if (timer) clearTimeout(timer)
    outer?.removeEventListener('abort', onOuterAbort)
  }

  let payload: ApiResponse<T> | null = null
  try {
    payload = (await resp.json()) as ApiResponse<T>
  } catch {
    payload = null
  }

  // 只有管理员会话失效才触发全局登出;iCloud 会话失效(UPSTREAM_UNAUTHORIZED)只是业务错误
  if (resp.status === 401 && payload?.code !== 'UPSTREAM_UNAUTHORIZED') {
    onUnauthorized?.()
    unauthorizedHandler?.()
  }

  if (payload === null) {
    throw new ApiError(resp.status, 'INVALID_RESPONSE', '网络连接失败，请检查服务状态')
  }

  if (!resp.ok || payload.success === false) {
    throw new ApiError(
      resp.status,
      payload.code ?? 'INTERNAL_ERROR',
      payload.message ?? '请求失败',
    )
  }
  return payload.data as T
}

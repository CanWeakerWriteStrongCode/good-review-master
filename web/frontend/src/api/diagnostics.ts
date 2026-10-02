import type { DiagnosticsData } from './types'
import { BASE_URL, getAuthHeader, handleUnauthorized, request } from './index'

/**
 * 运行时诊断接口。
 *
 * 这个模块刻意不走 request<T>：那层是给 {code,data} 信封用的，
 * 而 pprof 的端点返回明文或二进制，没有信封可解。
 */

/** 运行时概况 + goroutine 分组。full=true 时附上栈全文（体积大，按需取）。 */
export function fetchDiagnostics(full = false): Promise<DiagnosticsData> {
  return request<DiagnosticsData>(`/diagnostics${full ? '?full=1' : ''}`)
}

/**
 * 取 pprof 明文视图（debug=1 或 2）。返回原始文本。
 *
 * 不需要 responseType: 'arraybuffer'：uni-app 的 parseResponseText 用 try/catch
 * 包着 JSON.parse，非 JSON 会原样返回字符串，所以文本响应用默认设置就能拿到原文。
 * dataType 显式设成 'text' 只是省掉那次注定失败、且可能对几 MB 栈白做一遍的 JSON.parse。
 */
export async function fetchPprofText(name: string, debug = 1): Promise<string> {
  const res = await uni.request({
    url: `${BASE_URL}/debug/pprof/${name}?debug=${debug}`,
    method: 'GET',
    header: getAuthHeader(),
    dataType: 'text',
  })
  if (res.statusCode === 401) {
    handleUnauthorized()
  }
  if (res.statusCode !== 200) {
    throw new Error(`pprof ${name} 返回 HTTP ${res.statusCode}`)
  }
  return typeof res.data === 'string' ? res.data : ''
}

/**
 * 取 pprof 二进制 profile，返回 ArrayBuffer（供 go tool pprof 离线出火焰图）。
 *
 * responseType 只能写 'text' 或 'arraybuffer'——这是 uni-app 的 RESPONSE_TYPE 常量，
 * 写 'blob' 之类会被**静默**降级成 text（源码里 RESPONSE_TYPE.indexOf(...) === -1 就回退），
 * 那时 res.data 会是乱码字符串而不是二进制，且不报错。
 */
export async function fetchPprofBinary(name: string, seconds?: number): Promise<ArrayBuffer> {
  const query = seconds ? `?seconds=${seconds}` : ''
  const res = await uni.request({
    url: `${BASE_URL}/debug/pprof/${name}${query}`,
    method: 'GET',
    header: getAuthHeader(),
    responseType: 'arraybuffer',
  })
  if (res.statusCode === 401) {
    handleUnauthorized()
  }
  if (res.statusCode !== 200) {
    throw new Error(`pprof ${name} 返回 HTTP ${res.statusCode}`)
  }
  if (!(res.data instanceof ArrayBuffer)) {
    throw new Error(`pprof ${name} 未返回二进制数据`)
  }
  return res.data
}

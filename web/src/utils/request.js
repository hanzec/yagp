import axios from 'axios'
import { ElMessage } from 'element-plus'
import 'element-plus/es/components/message/style/css'

const AUTH_REQUIRED_CODE = 40100

// 创建 axios 实例
const request = axios.create({
  baseURL: '/api/v1',
  adapter: 'fetch',
  fetchOptions: { redirect: 'manual' },
  timeout: 10000,
  headers: {
    'Content-Type': 'application/json'
  }
})

let loginNavigation

function redirectToLogin() {
  if (!loginNavigation) {
    const params = new URLSearchParams({ rd: window.location.href })
    window.location.replace(`/outpost.goauthentik.io/start?${params}`)
    // Keep callers from displaying a request error while the page navigates.
    loginNavigation = new Promise(() => {})
  }
  return loginNavigation
}

// 响应拦截器
request.interceptors.response.use(
  (response) => {
    // With manual redirects, the browser exposes ForwardAuth's redirect as status 0.
    if (response.status === 0) return redirectToLogin()

    const res = response.data
    // 业务状态码判断
    if (res.code !== 0) {
      if (!response.config.silentError) ElMessage.error(res.message || '请求失败')
      return Promise.reject(new Error(res.message || '请求失败'))
    }
    return res
  },
  (error) => {
    const { status, data } = error.response ?? {}
    if (status === 401 && (data?.code == null || data.code === AUTH_REQUIRED_CODE)) {
      return redirectToLogin()
    }

    // HTTP 错误处理
    let message = '网络错误'
    if (error.response) {
      switch (status) {
        case 403:
          message = data?.message || '拒绝访问'
          break
        case 404:
          message = '请求地址不存在'
          break
        default:
          message = data?.message || '请求失败'
      }
    }
    if (!error.config?.silentError) ElMessage.error(message)
    return Promise.reject(error)
  }
)

export default request

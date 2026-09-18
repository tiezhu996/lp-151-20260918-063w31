import axios from 'axios'

export interface ApiResponse<T = unknown> {
  code: number
  message: string
  data: T
}

export const client = axios.create({
  baseURL: '/api/v1',
  timeout: 15000,
})

client.interceptors.request.use((config) => {
  const token = localStorage.getItem('gbtreehole_token')
  if (token) {
    config.headers.Authorization = `Bearer ${token}`
  }
  return config
})

client.interceptors.response.use(
  (response) => response,
  (error) => {
    if (error.response?.status === 401) {
      localStorage.removeItem('gbtreehole_token')
    }
    return Promise.reject(error)
  },
)

export async function request<T>(method: 'get' | 'post' | 'delete', url: string, data?: unknown): Promise<T> {
  let response
  try {
    response = await client.request<ApiResponse<T>>({
      method,
      url,
      ...(method === 'get' ? { params: data } : { data }),
    })
  } catch (err) {
    if (axios.isAxiosError(err) && err.response?.data?.message) {
      throw new Error(err.response.data.message)
    }
    throw err
  }
  if (response.data.code !== 0) {
    throw new Error(response.data.message || '请求失败')
  }
  return response.data.data
}

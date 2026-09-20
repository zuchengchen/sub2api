import { apiClient } from './client'

export interface PelicanTestItem {
  id: number
  status: string
  html: string
  model: string
  group_name: string
  reasoning_effort: string
  created_at: string
  finished_at?: string | null
  duration_ms: number
}

export interface PelicanTestPage {
  items: PelicanTestItem[]
  total: number
  page: number
  page_size: number
}

export const pelicanTestsAPI = {
  async list(page = 1, pageSize = 12) {
    return (await apiClient.get<PelicanTestPage>('/pelican-tests', { params: { page, page_size: pageSize } })).data
  }
}

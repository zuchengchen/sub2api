import { apiClient } from './client'

export interface PublicGuide {
  content: string
  version: number
  updated_at: string
  has_custom_content: boolean
}

export async function getPublicGuide(): Promise<PublicGuide> {
  const { data } = await apiClient.get<PublicGuide>('/settings/guide')
  return data
}

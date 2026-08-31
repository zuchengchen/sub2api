import { apiClient } from './client'

/** One published chapter. `slug` is the permanent anchor id on /guide. */
export interface PublicGuideChapter {
  slug: string
  title: string
  content: string
}

export interface PublicGuide {
  /**
   * The whole guide as one document. Retained so a guide published before
   * chapters existed still renders; new publishes populate `chapters`.
   */
  content?: string
  chapters?: PublicGuideChapter[]
  version: number
  updated_at: string
  has_custom_content: boolean
}

export async function getPublicGuide(): Promise<PublicGuide> {
  const { data } = await apiClient.get<PublicGuide>('/settings/guide')
  return data
}

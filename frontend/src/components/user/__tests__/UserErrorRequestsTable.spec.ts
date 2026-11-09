import { afterEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, shallowMount } from '@vue/test-utils'
import UserErrorRequestsTable from '../UserErrorRequestsTable.vue'
import type { UserErrorRequest } from '@/types'

vi.mock('vue-i18n', async (importOriginal) => {
  const actual = await importOriginal<typeof import('vue-i18n')>()
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})
enableAutoUnmount(afterEach)

describe('UserErrorRequestsTable final request status', () => {
  it.each([
    { final: 499, expected: '499' },
    { final: undefined, expected: '503' },
  ])('shows $expected from the request outcome', ({ final, expected }) => {
    const row = { id: 1, status_code: 503, final_request_status: final } as UserErrorRequest
    const wrapper = shallowMount(UserErrorRequestsTable, {
      props: { rows: [row], total: 1, loading: false, page: 1, pageSize: 20 },
      global: { stubs: {
        DataTable: { props: ['data'], template: '<div><template v-for="row in data"><slot name="cell-status" :row="row" /></template></div>' },
        Pagination: true, UserErrorDetailModal: true, IpGeoBatchToolbar: true,
      } },
    })
    expect(wrapper.find('span').text()).toBe(expected)
  })
})

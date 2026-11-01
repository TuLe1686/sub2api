import { beforeEach, describe, expect, it, vi } from 'vitest'

const { put, deleteRequest } = vi.hoisted(() => ({
  put: vi.fn(),
  deleteRequest: vi.fn()
}))

vi.mock('@/api/client', () => ({
  apiClient: {
    put,
    delete: deleteRequest
  }
}))

import { deletePlan, update } from '@/api/admin/scheduledTests'

const restoreOwnedAccount = { restore_owned_account: true } as const

describe('admin scheduled tests API', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    put.mockResolvedValue({ data: { id: 9, enabled: false } })
    deleteRequest.mockResolvedValue({ data: undefined })
  })

  it('encodes explicit ownership restoration for disabling and deleting plans', async () => {
    await update(9, { enabled: false }, restoreOwnedAccount)
    await deletePlan(9, restoreOwnedAccount)

    expect(put).toHaveBeenCalledWith(
      '/admin/scheduled-test-plans/9',
      { enabled: false },
      { params: { restore_owned_account: true } }
    )
    expect(deleteRequest).toHaveBeenCalledWith(
      '/admin/scheduled-test-plans/9',
      { params: { restore_owned_account: true } }
    )
  })
})

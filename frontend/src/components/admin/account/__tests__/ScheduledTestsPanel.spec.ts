import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import ScheduledTestsPanel from '../ScheduledTestsPanel.vue'
import type { ScheduledTestPlan, ScheduledTestResult } from '@/types'

const { listByAccount, listResults, create, update, deletePlan, showError, showSuccess } = vi.hoisted(() => ({
  listByAccount: vi.fn(),
  listResults: vi.fn(),
  create: vi.fn(),
  update: vi.fn(),
  deletePlan: vi.fn(),
  showError: vi.fn(),
  showSuccess: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    scheduledTests: {
      listByAccount,
      listResults,
      create,
      update,
      delete: deletePlan
    }
  }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError, showSuccess })
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => ({
        'admin.scheduledTests.consecutiveTimeoutCount': 'Consecutive timeouts',
        'admin.scheduledTests.runMode': 'Run mode',
        'admin.scheduledTests.classification': 'Classification',
        'admin.scheduledTests.protectionAction': 'Protection action',
        'admin.scheduledTests.blockedReason': 'Blocked reason',
        'admin.scheduledTests.ownsInactiveAccount': 'Owns inactive account',
        'admin.scheduledTests.confirmRestoreOwnership': 'Restore the account and release ownership?',
        'admin.scheduledTests.confirmDelete': 'Delete this plan?',
        'admin.scheduledTests.confirmDeleteWithOwnership': 'Delete this plan, restore the account, and release ownership?',
        'admin.scheduledTests.invalidRetryDelays': 'Retry delays must be up to 5 integers from 0 to 300.',
        'admin.scheduledTests.invalidTimeoutProtectionNumbers': 'Invalid timeout protection numbers.',
        'admin.scheduledTests.effectiveTimeoutProtectionMode': 'Effective mode',
        'admin.scheduledTests.timeoutProtectionOverrideReason': 'Override reason',
        'admin.scheduledTests.timeoutProtectionOverrideReasons.force_shadow': 'Forced shadow mode',
        'admin.scheduledTests.runModes.recovery': 'Recovery probe',
        'admin.scheduledTests.classifications.timeout': 'Timeout',
        'admin.scheduledTests.protectionActions.blocked': 'Blocked',
        'admin.scheduledTests.blockedReasons.account_already_owned': 'Account owned by another plan'
      })[key] || key
    })
  }
})

const plan: ScheduledTestPlan = {
  id: 9,
  account_id: 42,
  model_id: 'gpt-5.4',
  cron_expression: '*/30 * * * *',
  enabled: true,
  max_results: 100,
  auto_recover: true,
  timeout_protection_mode: 'enforce',
  effective_timeout_protection_mode: 'shadow',
  timeout_protection_override_reason: 'force_shadow',
  timeout_seconds: 120,
  consecutive_timeout_threshold: 3,
  retry_delays_seconds: [5, 15, 30],
  consecutive_timeout_count: 7,
  owns_inactive_account: true,
  last_run_at: null,
  next_run_at: null,
  created_at: '2026-08-15T00:00:00Z',
  updated_at: '2026-08-15T00:00:00Z'
}

const result: ScheduledTestResult = {
  id: 91,
  plan_id: 9,
  status: 'failed',
  response_text: '',
  error_message: 'ownership conflict',
  latency_ms: 120000,
  run_mode: 'recovery',
  attempt_count: 2,
  classification: 'timeout',
  protection_action: 'blocked',
  blocked_reason: 'account_already_owned',
  started_at: '2026-08-15T00:00:00Z',
  finished_at: '2026-08-15T00:02:00Z',
  created_at: '2026-08-15T00:02:00Z'
}

const mountedWrappers: Array<ReturnType<typeof mount>> = []

function mountPanel() {
  const wrapper = mount(ScheduledTestsPanel, {
    props: {
      show: false,
      accountId: 42,
      modelOptions: [{ value: 'gpt-5.4', label: 'GPT-5.4' }]
    },
    global: {
      stubs: {
        BaseDialog: {
          props: ['show'],
          template: '<div v-if="show"><slot /></div>'
        },
        ConfirmDialog: {
          inheritAttrs: false,
          props: ['show', 'message'],
          emits: ['confirm', 'cancel'],
          template: '<div v-if="show" :data-testid="$attrs[\'data-testid\']"><span class="confirm-message">{{ message }}</span><button class="confirm-action" @click="$emit(\'confirm\')">confirm</button><button class="cancel-action" @click="$emit(\'cancel\')">cancel</button></div>'
        },
        HelpTooltip: { template: '<span><slot /><slot name="trigger" /></span>' },
        Select: {
          inheritAttrs: false,
          props: ['modelValue', 'options'],
          emits: ['update:modelValue'],
          template: '<select v-bind="$attrs" :value="modelValue" @change="$emit(\'update:modelValue\', $event.target.value)"><option v-for="option in options" :key="option.value" :value="option.value">{{ option.label }}</option></select>'
        },
        Input: {
          inheritAttrs: false,
          props: ['modelValue'],
          emits: ['update:modelValue'],
          template: '<input v-bind="$attrs" :value="modelValue" @input="$emit(\'update:modelValue\', $event.target.value)" />'
        },
        Toggle: {
          inheritAttrs: false,
          props: ['modelValue'],
          emits: ['update:modelValue'],
          template: '<input v-bind="$attrs" type="checkbox" :checked="modelValue" @change="$emit(\'update:modelValue\', $event.target.checked)" />'
        },
        Icon: true
      }
    }
  })
  mountedWrappers.push(wrapper)
  return wrapper
}

describe('ScheduledTestsPanel', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    listByAccount.mockResolvedValue([plan])
    listResults.mockResolvedValue([result])
  })

  afterEach(() => {
    mountedWrappers.splice(0).forEach((wrapper) => wrapper.unmount())
    vi.useRealTimers()
  })

  it('shows timeout ownership status from plans and results', async () => {
    const wrapper = mountPanel()
    await wrapper.setProps({ show: true })
    await flushPromises()

    expect(wrapper.text()).toContain('Consecutive timeouts')
    expect(wrapper.text()).toContain('7')

    await wrapper.get('.cursor-pointer').trigger('click')
    await flushPromises()

    expect(wrapper.text()).toContain('Effective mode')
    expect(wrapper.text()).toContain('admin.scheduledTests.timeoutProtectionModes.shadow')
    expect(wrapper.text()).toContain('Override reason')
    expect(wrapper.text()).toContain('Forced shadow mode')

    expect(wrapper.text()).toContain('Run mode')
    expect(wrapper.text()).toContain('Recovery probe')
    expect(wrapper.text()).toContain('Classification')
    expect(wrapper.text()).toContain('Timeout')
    expect(wrapper.text()).toContain('Protection action')
    expect(wrapper.text()).toContain('Blocked')
    expect(wrapper.text()).toContain('Blocked reason')
    expect(wrapper.text()).toContain('Account owned by another plan')
    expect(wrapper.text()).toContain('Owns inactive account')
  })

  it('creates a plan with timeout protection and parsed retry delays', async () => {
    listByAccount.mockResolvedValue([])
    create.mockResolvedValue(plan)
    const wrapper = mountPanel()
    await wrapper.setProps({ show: true })
    await flushPromises()

    const addButton = wrapper.findAll('button').find((button) => button.text().includes('admin.scheduledTests.addPlan'))
    expect(addButton).toBeTruthy()
    await addButton!.trigger('click')

    await wrapper.get('[data-testid="new-model-id"]').setValue('gpt-5.4')
    await wrapper.get('[data-testid="new-cron-expression"]').setValue('*/10 * * * *')
    await wrapper.get('[data-testid="new-timeout-protection-mode"]').setValue('enforce')
    await wrapper.get('[data-testid="new-timeout-seconds"]').setValue('90')
    await wrapper.get('[data-testid="new-consecutive-timeout-threshold"]').setValue('4')
    await wrapper.get('[data-testid="new-retry-delays-seconds"]').setValue('5, 20, 60')

    const saveButton = wrapper.findAll('button').find((button) => button.text().includes('common.save'))
    expect(saveButton).toBeTruthy()
    await saveButton!.trigger('click')
    await flushPromises()

    expect(create).toHaveBeenCalledWith(expect.objectContaining({
      account_id: 42,
      model_id: 'gpt-5.4',
      cron_expression: '*/10 * * * *',
      timeout_protection_mode: 'enforce',
      timeout_seconds: 90,
      consecutive_timeout_threshold: 4,
      retry_delays_seconds: [5, 20, 60]
    }))
  })

  it.each(['5, invalid, 60', '5, -1, 60', '5, 301, 60'])(
    'rejects invalid retry delays without filtering or submitting: %s',
    async (retryDelays) => {
      listByAccount.mockResolvedValue([])
      const wrapper = mountPanel()
      await wrapper.setProps({ show: true })
      await flushPromises()

      await wrapper.findAll('button').find((button) => button.text().includes('admin.scheduledTests.addPlan'))!.trigger('click')
      await wrapper.get('[data-testid="new-model-id"]').setValue('gpt-5.4')
      await wrapper.get('[data-testid="new-cron-expression"]').setValue('*/10 * * * *')
      await wrapper.get('[data-testid="new-retry-delays-seconds"]').setValue(retryDelays)
      await wrapper.findAll('button').find((button) => button.text().includes('common.save'))!.trigger('click')
      await flushPromises()

      expect(create).not.toHaveBeenCalled()
      expect(showError).toHaveBeenCalledWith('Retry delays must be up to 5 integers from 0 to 300.')
    }
  )

  it('rejects more than five edit retry delays before ownership confirmation', async () => {
    const wrapper = mountPanel()
    await wrapper.setProps({ show: true })
    await flushPromises()

    await wrapper.get('button[title="admin.scheduledTests.editPlan"]').trigger('click')
    const editForm = wrapper.get('[data-testid="edit-plan-form"]')
    await editForm.get('[data-testid="edit-retry-delays-seconds"]').setValue('1, 2, 3, 4, 5, 6')
    await editForm.get('input[type="checkbox"]').setValue(false)
    await editForm.findAll('button').find((button) => button.text().includes('common.save'))!.trigger('click')

    expect(update).not.toHaveBeenCalled()
    expect(wrapper.find('[data-testid="confirm-restore-ownership"]').exists()).toBe(false)
    expect(showError).toHaveBeenCalledWith('Retry delays must be up to 5 integers from 0 to 300.')
  })

  it('submits an explicit empty retry delay array when the field is blank', async () => {
    listByAccount.mockResolvedValue([])
    create.mockResolvedValue(plan)
    const wrapper = mountPanel()
    await wrapper.setProps({ show: true })
    await flushPromises()

    await wrapper.findAll('button').find((button) => button.text().includes('admin.scheduledTests.addPlan'))!.trigger('click')
    await wrapper.get('[data-testid="new-model-id"]').setValue('gpt-5.4')
    await wrapper.get('[data-testid="new-cron-expression"]').setValue('*/10 * * * *')
    await wrapper.get('[data-testid="new-retry-delays-seconds"]').setValue('   ')
    await wrapper.findAll('button').find((button) => button.text().includes('common.save'))!.trigger('click')
    await flushPromises()

    expect(create).toHaveBeenCalledWith(expect.objectContaining({ retry_delays_seconds: [] }))
  })

  it('omits blank timeout numbers but preserves explicit zero values', async () => {
    listByAccount.mockResolvedValue([])
    create.mockResolvedValue(plan)
    const wrapper = mountPanel()
    await wrapper.setProps({ show: true })
    await flushPromises()

    await wrapper.findAll('button').find((button) => button.text().includes('admin.scheduledTests.addPlan'))!.trigger('click')
    await wrapper.get('[data-testid="new-model-id"]').setValue('gpt-5.4')
    await wrapper.get('[data-testid="new-cron-expression"]').setValue('*/10 * * * *')
    await wrapper.get('[data-testid="new-timeout-seconds"]').setValue('   ')
    await wrapper.get('[data-testid="new-consecutive-timeout-threshold"]').setValue('0')
    await wrapper.findAll('button').find((button) => button.text().includes('common.save'))!.trigger('click')
    await flushPromises()

    const request = create.mock.calls[0][0]
    expect(request).not.toHaveProperty('timeout_seconds')
    expect(request).toHaveProperty('consecutive_timeout_threshold', 0)
  })

  it('rejects invalid timeout numbers before submitting', async () => {
    listByAccount.mockResolvedValue([])
    const wrapper = mountPanel()
    await wrapper.setProps({ show: true })
    await flushPromises()

    await wrapper.findAll('button').find((button) => button.text().includes('admin.scheduledTests.addPlan'))!.trigger('click')
    await wrapper.get('[data-testid="new-model-id"]').setValue('gpt-5.4')
    await wrapper.get('[data-testid="new-cron-expression"]').setValue('*/10 * * * *')
    await wrapper.get('[data-testid="new-timeout-seconds"]').setValue('1.5')
    await wrapper.findAll('button').find((button) => button.text().includes('common.save'))!.trigger('click')
    await flushPromises()

    expect(create).not.toHaveBeenCalled()
    expect(showError).toHaveBeenCalledWith('Invalid timeout protection numbers.')
  })

  it('requires explicit confirmation before disabling a plan that owns an inactive account', async () => {
    update.mockResolvedValue({ ...plan, enabled: false, owns_inactive_account: false })
    const wrapper = mountPanel()
    await wrapper.setProps({ show: true })
    await flushPromises()

    const enabledToggle = wrapper.findAll('input[type="checkbox"]').find((input) => input.element.checked)
    expect(enabledToggle).toBeTruthy()
    await enabledToggle!.setValue(false)

    const confirmation = wrapper.get('[data-testid="confirm-restore-ownership"]')
    expect(confirmation.text()).toContain('Restore the account and release ownership?')
    expect(update).not.toHaveBeenCalled()

    await confirmation.get('.cancel-action').trigger('click')
    expect(update).not.toHaveBeenCalled()
    const restoredToggle = wrapper.findAll('input[type="checkbox"]').find((input) => input.element.checked)
    expect(restoredToggle).toBeTruthy()

    await restoredToggle!.setValue(false)
    await wrapper.get('[data-testid="confirm-restore-ownership"] .confirm-action').trigger('click')
    await flushPromises()

    expect(update).toHaveBeenCalledWith(9, { enabled: false }, { restore_owned_account: true })
  })

  it('requires explicit confirmation before an edit disables an owned plan', async () => {
    update.mockResolvedValue({ ...plan, enabled: false, owns_inactive_account: false })
    const wrapper = mountPanel()
    await wrapper.setProps({ show: true })
    await flushPromises()

    await wrapper.get('button[title="admin.scheduledTests.editPlan"]').trigger('click')
    const editForm = wrapper.get('[data-testid="edit-plan-form"]')
    await editForm.get('input[type="checkbox"]').setValue(false)
    await editForm.findAll('button').find((button) => button.text().includes('common.save'))!.trigger('click')

    expect(update).not.toHaveBeenCalled()
    await wrapper.get('[data-testid="confirm-restore-ownership"] .cancel-action').trigger('click')
    expect(update).not.toHaveBeenCalled()
    expect(wrapper.find('[data-testid="edit-plan-form"]').exists()).toBe(true)

    await editForm.findAll('button').find((button) => button.text().includes('common.save'))!.trigger('click')
    await wrapper.get('[data-testid="confirm-restore-ownership"] .confirm-action').trigger('click')
    await flushPromises()

    expect(update).toHaveBeenCalledWith(
      9,
      expect.objectContaining({ enabled: false }),
      { restore_owned_account: true }
    )
  })

  it('uses normal requests without ownership and offers explicit restore after a conflict', async () => {
    const unownedPlan = { ...plan, owns_inactive_account: false }
    listByAccount.mockResolvedValue([unownedPlan])
    update.mockRejectedValueOnce({ response: { status: 409 } })
    update.mockResolvedValueOnce({ ...unownedPlan, enabled: false })
    const wrapper = mountPanel()
    await wrapper.setProps({ show: true })
    await flushPromises()

    const enabledToggle = wrapper.findAll('input[type="checkbox"]').find((input) => input.element.checked)
    await enabledToggle!.setValue(false)
    await flushPromises()

    expect(update).toHaveBeenNthCalledWith(1, 9, { enabled: false })
    expect(wrapper.find('[data-testid="confirm-restore-ownership"]').exists()).toBe(true)

    await wrapper.get('[data-testid="confirm-restore-ownership"] .confirm-action').trigger('click')
    await flushPromises()

    expect(update).toHaveBeenNthCalledWith(2, 9, { enabled: false }, { restore_owned_account: true })
  })

  it('uses ownership-aware delete messages and omits restore for unowned plans', async () => {
    deletePlan.mockResolvedValue(undefined)
    const wrapper = mountPanel()
    await wrapper.setProps({ show: true })
    await flushPromises()

    await wrapper.get('button[title="admin.scheduledTests.deletePlan"]').trigger('click')
    expect(wrapper.get('[data-testid="confirm-delete"] .confirm-message').text()).toContain('restore the account')
    await wrapper.get('[data-testid="confirm-delete"] .confirm-action').trigger('click')
    await flushPromises()
    expect(deletePlan).toHaveBeenCalledWith(9, { restore_owned_account: true })

    listByAccount.mockResolvedValue([{ ...plan, id: 10, owns_inactive_account: false }])
    await wrapper.setProps({ show: false })
    await wrapper.setProps({ show: true })
    await flushPromises()
    await wrapper.get('button[title="admin.scheduledTests.deletePlan"]').trigger('click')
    expect(wrapper.get('[data-testid="confirm-delete"] .confirm-message').text()).toBe('Delete this plan?')
    await wrapper.get('[data-testid="confirm-delete"] .confirm-action').trigger('click')
    await flushPromises()
    expect(deletePlan).toHaveBeenLastCalledWith(10)
  })

  it('requires explicit restore confirmation after an unowned delete conflicts', async () => {
    listByAccount.mockResolvedValue([{ ...plan, owns_inactive_account: false }])
    deletePlan.mockRejectedValueOnce({ response: { status: 409 } })
    deletePlan.mockRejectedValueOnce({ response: { status: 409 } })
    deletePlan.mockResolvedValueOnce(undefined)
    const wrapper = mountPanel()
    await wrapper.setProps({ show: true })
    await flushPromises()

    await wrapper.get('button[title="admin.scheduledTests.deletePlan"]').trigger('click')
    await wrapper.get('[data-testid="confirm-delete"] .confirm-action').trigger('click')
    await flushPromises()

    expect(deletePlan).toHaveBeenNthCalledWith(1, 9)
    expect(wrapper.get('[data-testid="confirm-delete"] .confirm-message').text()).toContain('restore the account')

    await wrapper.get('[data-testid="confirm-delete"] .cancel-action').trigger('click')
    expect(deletePlan).toHaveBeenCalledTimes(1)

    await wrapper.get('button[title="admin.scheduledTests.deletePlan"]').trigger('click')
    await wrapper.get('[data-testid="confirm-delete"] .confirm-action').trigger('click')
    await flushPromises()
    await wrapper.get('[data-testid="confirm-delete"] .confirm-action').trigger('click')
    await flushPromises()

    expect(deletePlan).toHaveBeenNthCalledWith(3, 9, { restore_owned_account: true })
  })

  it('ignores a late response after switching accounts', async () => {
    let resolveAccount42: ((plans: ScheduledTestPlan[]) => void) | undefined
    listByAccount
      .mockImplementationOnce(() => new Promise<ScheduledTestPlan[]>((resolve) => {
        resolveAccount42 = resolve
      }))
      .mockResolvedValueOnce([{ ...plan, id: 10, account_id: 43, model_id: 'gpt-5.5' }])

    const wrapper = mountPanel()
    await wrapper.setProps({ show: true })
    await wrapper.setProps({ accountId: 43 })
    await flushPromises()

    expect(wrapper.text()).toContain('gpt-5.5')
    resolveAccount42?.([plan])
    await flushPromises()

    expect(wrapper.text()).toContain('gpt-5.5')
    expect(wrapper.text()).not.toContain('gpt-5.4')
    expect(listByAccount).toHaveBeenNthCalledWith(1, 42)
    expect(listByAccount).toHaveBeenNthCalledWith(2, 43)
  })

  it('polls every 15 seconds while open and supports manual refresh', async () => {
    vi.useFakeTimers()
    const wrapper = mountPanel()
    await wrapper.setProps({ show: true })
    await flushPromises()
    expect(listByAccount).toHaveBeenCalledTimes(1)

    await wrapper.get('.cursor-pointer').trigger('click')
    await flushPromises()
    expect(listResults).toHaveBeenCalledTimes(1)

    await vi.advanceTimersByTimeAsync(15_000)
    await flushPromises()
    expect(listByAccount).toHaveBeenCalledTimes(2)
    expect(listResults).toHaveBeenCalledTimes(2)

    await wrapper.get('[data-testid="refresh-scheduled-tests"]').trigger('click')
    await flushPromises()
    expect(listByAccount).toHaveBeenCalledTimes(3)
    expect(listResults).toHaveBeenCalledTimes(3)

    await wrapper.setProps({ show: false })
    await vi.advanceTimersByTimeAsync(15_000)
    expect(listByAccount).toHaveBeenCalledTimes(3)
  })
})

// @vitest-environment jsdom
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import AccountsEnhancedView from '../AccountsEnhancedView.vue'

const { list, getConfig, updateConfig, showError } = vi.hoisted(() => ({
  list: vi.fn(), getConfig: vi.fn(), updateConfig: vi.fn(), showError: vi.fn()
}))
vi.mock('@/components/layout/AppLayout.vue', () => ({ default: { template: '<div><slot /></div>' } }))
vi.mock('@/api/admin/accounts', () => ({ list, getEnhancedControl: getConfig, updateEnhancedControl: updateConfig }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showError, showSuccess: vi.fn() }) }))
vi.mock('vue-i18n', async (importOriginal) => ({ ...await importOriginal<typeof import('vue-i18n')>(), useI18n: () => ({ t: (key: string) => key }) }))

function config(seconds = 600) {
  return { cache_rate: { mode: 'off' }, ttft: { mode: 'off' }, reasoning_effort: { mode: 'off', rules: [] }, stream_recovery: { enabled: false, cooldown_seconds: seconds } }
}
const mountView = () => mount(AccountsEnhancedView, { global: { stubs: { AppLayout: { template: '<div><slot /></div>' }, Icon: true } } })
function saveButton(wrapper: ReturnType<typeof mountView>) {
  return wrapper.findAll('button').find(b => b.text() === 'common.save')!
}

describe('AccountsEnhancedView stream recovery', () => {
  beforeEach(() => {
    vi.resetAllMocks()
    list.mockResolvedValue({ items: [{ id: 1, name: 'OpenAI key', platform: 'openai', type: 'apikey' }] })
    getConfig.mockResolvedValue(config())
    updateConfig.mockResolvedValue(config())
  })

  it('shows controls only for OpenAI API-key accounts, with defaults and supported scope', async () => {
    list.mockResolvedValue({ items: [
      { id: 1, platform: 'openai', type: 'apikey' },
      { id: 2, platform: 'openai', type: 'oauth' },
      { id: 3, platform: 'anthropic', type: 'apikey' }
    ] })
    getConfig.mockResolvedValue({ cache_rate: { mode: 'off' }, ttft: { mode: 'off' } })
    const wrapper = mountView()
    await flushPromises()
    expect(wrapper.findAll('input[type="checkbox"]')).toHaveLength(1)
    const duration = wrapper.get('input[type="number"][max="86400"]')
    expect((duration.element as HTMLInputElement).value).toBe('600')
    expect(wrapper.text()).toContain('admin.accountsEnhanced.streamRecoveryScope')
    expect(wrapper.text()).toContain('admin.accountsEnhanced.streamRecoveryZeroHint')
    wrapper.unmount()
  })

  it('preserves explicit zero and fills the saved backend values back into the form', async () => {
    getConfig.mockResolvedValue(config(0))
    updateConfig.mockResolvedValue({ ...config(42), stream_recovery: { enabled: true, cooldown_seconds: 42 } })
    const wrapper = mountView()
    await flushPromises()
    expect((wrapper.get('input[type="number"][max="86400"]').element as HTMLInputElement).value).toBe('0')
    await wrapper.get('input[type="checkbox"]').setValue(true)
    await saveButton(wrapper).trigger('click')
    await flushPromises()
    expect(updateConfig).toHaveBeenCalledWith(1, expect.objectContaining({ stream_recovery: { enabled: true, cooldown_seconds: 0 } }))
    expect((wrapper.get('input[type="number"][max="86400"]').element as HTMLInputElement).value).toBe('42')
    expect(saveButton(wrapper).attributes('disabled')).toBeDefined()
    wrapper.unmount()
  })

  it('blocks noninteger and out-of-range durations before PUT', async () => {
    const wrapper = mountView()
    await flushPromises()
    for (const value of ['-1', '86401', '1.5', '']) {
      await wrapper.get('input[type="number"][max="86400"]').setValue(value)
      await saveButton(wrapper).trigger('click')
      await flushPromises()
    }
    expect(updateConfig).not.toHaveBeenCalled()
    expect(showError).toHaveBeenCalledTimes(4)
    wrapper.unmount()
  })

  it('toasts PUT errors and leaves unsaved changes retryable', async () => {
    updateConfig.mockRejectedValue({ message: 'write failed' })
    const wrapper = mountView()
    await flushPromises()
    await wrapper.get('input[type="checkbox"]').setValue(true)
    await saveButton(wrapper).trigger('click')
    await flushPromises()
    expect(showError).toHaveBeenCalledWith('write failed')
    expect(saveButton(wrapper).attributes('disabled')).toBeUndefined()
    wrapper.unmount()
  })

  it('toasts load errors without unhandled promise rejection', async () => {
    list.mockRejectedValue(new Error('read failed'))
    const wrapper = mountView()
    await flushPromises()
    expect(showError).toHaveBeenCalledWith('read failed')
    wrapper.unmount()
  })
})

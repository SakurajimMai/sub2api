import { defineComponent } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import OpsQuotaResetLinks from '../OpsQuotaResetLinks.vue'

const mocks = vi.hoisted(() => ({
  listRules: vi.fn(),
  listExecutions: vi.fn(),
  listEvents: vi.fn(),
  listAccounts: vi.fn(),
  listGenericAccounts: vi.fn(),
  listGroups: vi.fn(),
  checkRule: vi.fn(),
  showError: vi.fn(),
  showSuccess: vi.fn(),
}))

vi.mock('@/api', () => ({
  adminAPI: {
    accounts: {
      list: mocks.listGenericAccounts,
    },
    groups: {
      getAll: mocks.listGroups,
    },
  },
}))

vi.mock('@/api/admin/ops', () => ({
  opsAPI: {
    listOpenAIWeeklyQuotaResetRules: mocks.listRules,
    listOpenAIWeeklyQuotaResetExecutions: mocks.listExecutions,
    listOpenAIWeeklyQuotaResetEvents: mocks.listEvents,
    listOpenAIWeeklyQuotaResetAccounts: mocks.listAccounts,
    checkOpenAIWeeklyQuotaResetRule: mocks.checkRule,
    createOpenAIWeeklyQuotaResetRule: vi.fn(),
    updateOpenAIWeeklyQuotaResetRule: vi.fn(),
    deleteOpenAIWeeklyQuotaResetRule: vi.fn(),
  },
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError: mocks.showError,
    showSuccess: mocks.showSuccess,
  }),
}))

vi.mock('vue-i18n', async (importOriginal) => {
  const actual = await importOriginal<typeof import('vue-i18n')>()
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key === 'common.unknown' ? '未知' : key,
    }),
  }
})

const BaseDialogStub = defineComponent({
  name: 'BaseDialog',
  props: {
    show: {
      type: Boolean,
      default: false,
    },
  },
  template: '<section v-if="show" data-testid="editor"><slot /><slot name="footer" /></section>',
})

const SelectStub = defineComponent({
  name: 'AccountSelectStub',
  props: {
    options: {
      type: Array,
      default: () => [],
    },
  },
  template: `
    <ul class="select-options">
      <li v-for="option in options" :key="String(option.value)">{{ option.label }}</li>
    </ul>
  `,
})

const rule = {
  id: 7,
  name: '周额度联动规则',
  description: '',
  enabled: true,
  source_account_id: 17,
  source_account_name: '本地 OpenAI 主账号',
  target_group_id: 3,
  target_group_name: 'Pro 用户组',
  created_at: '2026-09-01T00:00:00Z',
  updated_at: '2026-09-01T00:00:00Z',
}

function mountComponent() {
  return mount(OpsQuotaResetLinks, {
    global: {
      stubs: {
        BaseDialog: BaseDialogStub,
        ConfirmDialog: true,
        Select: SelectStub,
        Toggle: true,
      },
    },
  })
}

describe('OpsQuotaResetLinks', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    mocks.listRules.mockResolvedValue([rule])
    mocks.listExecutions.mockResolvedValue([])
    mocks.listEvents.mockResolvedValue([])
    mocks.listAccounts.mockResolvedValue([])
    mocks.listGenericAccounts.mockResolvedValue({
      items: [],
      total: 0,
      page: 1,
      page_size: 500,
      pages: 0,
    })
    mocks.listGroups.mockResolvedValue([])
    mocks.checkRule.mockResolvedValue({ outcome: 'unchanged' })
  })

  it('检测失败后尽力刷新规则并呈现后端记录的新状态', async () => {
    const checkError = {
      status: 502,
      message: 'OpenAI 周额度查询失败',
      reason: 'OPENAI_WEEKLY_QUOTA_QUERY_FAILED',
      request_id: 'req-check-7',
      metadata: { phase: 'upstream_query' },
    }
    mocks.checkRule.mockRejectedValue(checkError)
    mocks.listRules
      .mockResolvedValueOnce([rule])
      .mockResolvedValueOnce([{
        ...rule,
        last_error: '后端记录的检查失败',
      }])

    const wrapper = mountComponent()
    await flushPromises()

    const checkButton = wrapper
      .findAll('button')
      .find((button) => button.text() === 'admin.ops.quotaResetLinks.checkNow')
    expect(checkButton).toBeDefined()

    await checkButton!.trigger('click')
    await flushPromises()

    expect(mocks.checkRule).toHaveBeenCalledWith(rule.id)
    expect(mocks.listRules).toHaveBeenCalledTimes(2)
    expect(wrapper.text()).toContain('后端记录的检查失败')
  })

  it('检测失败后的规则刷新再次失败时仍只展示原始检测错误', async () => {
    const checkError = {
      status: 502,
      message: 'OpenAI 周额度查询失败',
      reason: 'OPENAI_WEEKLY_QUOTA_QUERY_FAILED',
      request_id: 'req-check-7',
      metadata: { phase: 'upstream_query' },
    }
    mocks.checkRule.mockRejectedValue(checkError)
    mocks.listRules
      .mockResolvedValueOnce([rule])
      .mockRejectedValueOnce(new Error('规则刷新失败'))

    const wrapper = mountComponent()
    await flushPromises()

    const checkButton = wrapper
      .findAll('button')
      .find((button) => button.text() === 'admin.ops.quotaResetLinks.checkNow')
    expect(checkButton).toBeDefined()

    await checkButton!.trigger('click')
    await flushPromises()

    expect(mocks.showError).toHaveBeenCalledTimes(1)
    expect(mocks.showError).toHaveBeenCalledWith(expect.stringContaining(checkError.message))
    expect(mocks.showError).toHaveBeenCalledWith(expect.stringContaining(checkError.reason))
    expect(mocks.showError).toHaveBeenCalledWith(expect.stringContaining('upstream_query'))
    expect(mocks.showError).toHaveBeenCalledWith(expect.stringContaining('HTTP 502'))
    expect(mocks.showError).toHaveBeenCalledWith(expect.stringContaining(checkError.request_id))
    expect(mocks.listRules).toHaveBeenCalledTimes(2)
  })

  it('使用专用安全 DTO 构造可辨识账户选项且不向 DOM 暴露敏感字段', async () => {
    mocks.listAccounts.mockResolvedValue([
      {
        local_account_id: 17,
        local_account_name: '本地 OpenAI 主账号',
        chatgpt_account_id: 'chatgpt-account-17',
        chatgpt_user_id: 'chatgpt-user-17',
        email: 'owner@example.com',
        plan_type: 'pro',
        identity_source: 'oauth',
        last_verified_at: '2026-09-01T08:00:00Z',
        supported: true,
        unsupported_reason: '',
        credentials: {
          access_token: 'secret-access-token',
          refresh_token: 'secret-refresh-token',
        },
        extra: {
          id_token: 'secret-id-token',
        },
        token: 'secret-generic-token',
      },
      {
        local_account_id: 18,
        local_account_name: '无邮箱账户',
        chatgpt_account_id: 'chatgpt-account-18',
        chatgpt_user_id: 'chatgpt-user-18',
        email: '',
        plan_type: 'plus',
        identity_source: 'agent_identity',
        last_verified_at: '2026-09-01T09:00:00Z',
        supported: true,
        unsupported_reason: '',
      },
    ])

    const wrapper = mountComponent()
    await flushPromises()

    expect(mocks.listAccounts).toHaveBeenCalledTimes(1)
    expect(mocks.listGenericAccounts).not.toHaveBeenCalled()

    const createButton = wrapper
      .findAll('button')
      .find((button) => button.text() === 'admin.ops.quotaResetLinks.create')
    expect(createButton).toBeDefined()
    await createButton!.trigger('click')

    const accountOptions = wrapper.findAll('.select-options')[0]
    expect(accountOptions.text()).toContain('17')
    expect(accountOptions.text()).toContain('本地 OpenAI 主账号')
    expect(accountOptions.text()).toContain('chatgpt-account-17')
    expect(accountOptions.text()).toContain('owner@example.com')
    expect(accountOptions.text()).toContain('chatgpt-user-17')
    expect(accountOptions.text()).toContain('pro')
    expect(accountOptions.text()).toContain('oauth')
    expect(accountOptions.text()).toContain('09-01')

    const missingEmailOption = accountOptions.findAll('li')[1]
    expect(missingEmailOption.text()).toContain('18')
    expect(missingEmailOption.text()).toContain('无邮箱账户')
    expect(missingEmailOption.text()).toContain('未知')
    expect(missingEmailOption.text()).toContain('chatgpt-user-18')
    expect(missingEmailOption.text()).toContain('plus')
    expect(missingEmailOption.text()).toContain('agent_identity')

    const html = wrapper.html()
    expect(html).not.toContain('secret-access-token')
    expect(html).not.toContain('secret-refresh-token')
    expect(html).not.toContain('secret-id-token')
    expect(html).not.toContain('secret-generic-token')
    expect(html).not.toContain('credentials')
    expect(html).not.toContain('extra')
    expect(html).not.toContain('access_token')
    expect(html).not.toContain('refresh_token')
    expect(html).not.toContain('id_token')
    expect(html).not.toContain('token')
  })
})

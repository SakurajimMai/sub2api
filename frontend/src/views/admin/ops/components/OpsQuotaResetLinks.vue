<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api'
import { opsAPI, type OpenAIWeeklyQuotaResetAccount, type OpenAIWeeklyQuotaResetEvent, type OpenAIWeeklyQuotaResetExecution, type OpenAIWeeklyQuotaResetRule, type OpenAIWeeklyQuotaResetRuleInput } from '@/api/admin/ops'
import BaseDialog from '@/components/common/BaseDialog.vue'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import Select, { type SelectOption } from '@/components/common/Select.vue'
import Toggle from '@/components/common/Toggle.vue'
import { useAppStore } from '@/stores/app'
import { formatDateTime } from '../utils/opsFormatters'

const { t } = useI18n()
const appStore = useAppStore()
const loading = ref(false)
const saving = ref(false)
const checkingId = ref<number | null>(null)
const rules = ref<OpenAIWeeklyQuotaResetRule[]>([])
const executions = ref<OpenAIWeeklyQuotaResetExecution[]>([])
const resetEvents = ref<OpenAIWeeklyQuotaResetEvent[]>([])
const accountOptions = ref<SelectOption[]>([])
const groupOptions = ref<SelectOption[]>([])
const showEditor = ref(false)
const editingId = ref<number | null>(null)
const pendingDelete = ref<OpenAIWeeklyQuotaResetRule | null>(null)

const draft = ref<OpenAIWeeklyQuotaResetRuleInput>(newDraft())

function newDraft(): OpenAIWeeklyQuotaResetRuleInput {
  return { name: '', description: '', enabled: true, source_account_id: 0, target_group_id: 0 }
}

const sortedRules = computed(() => [...rules.value].sort((a, b) => b.id - a.id))
const validDraft = computed(() => draft.value.name.trim().length > 0 && draft.value.source_account_id > 0 && draft.value.target_group_id > 0)

function safeErrorMessage(err: unknown, fallback: string) {
  if (err && typeof err === 'object' && 'message' in err) {
    const message = (err as { message?: unknown }).message
    if (typeof message === 'string' && message.trim()) return message.trim()
  }
  return fallback
}

function safeDiagnosticErrorMessage(err: unknown, fallback: string) {
  if (!err || typeof err !== 'object') return fallback
  const apiError = err as {
    message?: unknown
    reason?: unknown
    status?: unknown
    request_id?: unknown
    metadata?: unknown
  }
  const parts: string[] = [safeErrorMessage(err, fallback)]
  if (typeof apiError.reason === 'string' && apiError.reason.trim()) {
    parts.push(diagnosticLabel('reasons', apiError.reason.trim()))
  }
  if (typeof apiError.status === 'number' && Number.isFinite(apiError.status)) {
    parts.push(`HTTP ${apiError.status}`)
  }
  if (apiError.metadata && typeof apiError.metadata === 'object') {
    const metadata = apiError.metadata as Record<string, unknown>
    const stage = typeof metadata.stage === 'string' ? metadata.stage : metadata.phase
    if (typeof stage === 'string' && stage.trim()) {
      parts.push(diagnosticLabel('stages', stage.trim()))
    }
  }
  if (typeof apiError.request_id === 'string' && apiError.request_id.trim()) {
    parts.push(`request_id: ${apiError.request_id.trim()}`)
  }
  return [...new Set(parts)].join(' · ')
}

function accountOptionLabel(account: OpenAIWeeklyQuotaResetAccount) {
  const email = account.email?.trim() || t('common.unknown')
  const accountID = account.chatgpt_account_id?.trim() || t('common.unknown')
  const userID = account.chatgpt_user_id?.trim() || t('common.unknown')
  const plan = account.plan_type?.trim() || t('common.unknown')
  const verifiedAt = account.last_verified_at ? formatDateTime(account.last_verified_at) : t('common.unknown')
  return `#${account.local_account_id} · ${account.local_account_name} · ${accountID} · ${email} · ${userID} · ${plan} · ${account.identity_source} · ${verifiedAt}`
}

function statusClass(status: string) {
  if (status === 'succeeded' || status === 'confirmed') return 'bg-emerald-50 text-emerald-700 dark:bg-emerald-900/25 dark:text-emerald-300'
  if (status === 'retryable_failed' || status === 'permanent_failed' || status === 'rejected') return 'bg-red-50 text-red-700 dark:bg-red-900/25 dark:text-red-300'
  return 'bg-amber-50 text-amber-700 dark:bg-amber-900/25 dark:text-amber-300'
}

function diagnosticLabel(kind: 'stages' | 'reasons', value: string) {
  const key = `admin.ops.quotaResetLinks.${kind}.${value}`
  const translated = t(key)
  return translated === key ? value : translated
}

async function load() {
  loading.value = true
  try {
    const [ruleList, executionList, eventList, accounts, groups] = await Promise.all([
      opsAPI.listOpenAIWeeklyQuotaResetRules(),
      opsAPI.listOpenAIWeeklyQuotaResetExecutions({ limit: 50 }),
      opsAPI.listOpenAIWeeklyQuotaResetEvents({ limit: 50 }),
      opsAPI.listOpenAIWeeklyQuotaResetAccounts(),
      adminAPI.groups.getAll()
    ])
    rules.value = ruleList
    executions.value = executionList
    resetEvents.value = eventList
    accountOptions.value = accounts.map((account) => ({
      value: account.local_account_id,
      label: account.supported
        ? accountOptionLabel(account)
        : `${accountOptionLabel(account)} · ${t('admin.ops.quotaResetLinks.unsupported')}: ${account.unsupported_reason || t('common.unknown')}`,
      disabled: !account.supported
    }))
    groupOptions.value = groups.map((group) => ({ value: group.id, label: group.name }))
  } catch (err: any) {
    appStore.showError(safeErrorMessage(err, t('admin.ops.quotaResetLinks.loadFailed')))
  } finally {
    loading.value = false
  }
}

function openCreate() {
  editingId.value = null
  draft.value = newDraft()
  showEditor.value = true
}

function openEdit(rule: OpenAIWeeklyQuotaResetRule) {
  editingId.value = rule.id
  draft.value = {
    name: rule.name,
    description: rule.description || '',
    enabled: rule.enabled,
    source_account_id: rule.source_account_id,
    target_group_id: rule.target_group_id
  }
  showEditor.value = true
}

async function save() {
  if (!validDraft.value) {
    appStore.showError(t('admin.ops.quotaResetLinks.validation'))
    return
  }
  saving.value = true
  try {
    if (editingId.value) await opsAPI.updateOpenAIWeeklyQuotaResetRule(editingId.value, draft.value)
    else await opsAPI.createOpenAIWeeklyQuotaResetRule(draft.value)
    showEditor.value = false
    await load()
    appStore.showSuccess(t('admin.ops.quotaResetLinks.saveSuccess'))
  } catch (err: any) {
    appStore.showError(safeErrorMessage(err, t('admin.ops.quotaResetLinks.saveFailed')))
  } finally {
    saving.value = false
  }
}

async function checkNow(rule: OpenAIWeeklyQuotaResetRule) {
  checkingId.value = rule.id
  try {
    const result = await opsAPI.checkOpenAIWeeklyQuotaResetRule(rule.id)
    const successKey = result.zero_reason
      ? `admin.ops.quotaResetLinks.zeroReasons.${result.zero_reason}`
      : `admin.ops.quotaResetLinks.outcomes.${result.outcome}`
    appStore.showSuccess(t(successKey))
    await load()
  } catch (err: any) {
    const originalMessage = safeDiagnosticErrorMessage(err, t('admin.ops.quotaResetLinks.checkFailed'))
    try {
      rules.value = await opsAPI.listOpenAIWeeklyQuotaResetRules()
      executions.value = await opsAPI.listOpenAIWeeklyQuotaResetExecutions({ limit: 50 })
      resetEvents.value = await opsAPI.listOpenAIWeeklyQuotaResetEvents({ limit: 50 })
    } catch {
      // 状态刷新是 best-effort；原始检测错误优先。
    }
    appStore.showError(originalMessage)
  } finally {
    checkingId.value = null
  }
}

async function confirmDelete() {
  if (!pendingDelete.value) return
  try {
    await opsAPI.deleteOpenAIWeeklyQuotaResetRule(pendingDelete.value.id)
    pendingDelete.value = null
    await load()
    appStore.showSuccess(t('admin.ops.quotaResetLinks.deleteSuccess'))
  } catch (err: any) {
    appStore.showError(safeErrorMessage(err, t('admin.ops.quotaResetLinks.deleteFailed')))
  }
}

onMounted(load)
</script>

<template>
  <div class="rounded-3xl bg-white p-6 shadow-sm ring-1 ring-gray-900/5 dark:bg-dark-800 dark:ring-dark-700">
    <div class="mb-4 flex flex-wrap items-start justify-between gap-3">
      <div class="min-w-0">
        <h3 class="text-sm font-bold text-gray-900 dark:text-white">{{ t('admin.ops.quotaResetLinks.title') }}</h3>
        <p class="mt-1 max-w-3xl text-xs leading-5 text-gray-500 dark:text-gray-400">{{ t('admin.ops.quotaResetLinks.description') }}</p>
      </div>
      <div class="flex items-center gap-2">
        <button class="btn btn-sm btn-primary" :disabled="loading" @click="openCreate">{{ t('admin.ops.quotaResetLinks.create') }}</button>
        <button class="btn btn-sm btn-secondary" :disabled="loading" @click="load">{{ t('common.refresh') }}</button>
      </div>
    </div>

    <div v-if="loading" class="py-10 text-center text-sm text-gray-500 dark:text-gray-400">{{ t('common.loading') }}</div>
    <div v-else-if="sortedRules.length === 0" class="rounded-xl border border-dashed border-gray-200 p-8 text-center text-sm text-gray-500 dark:border-dark-700 dark:text-gray-400">
      {{ t('admin.ops.quotaResetLinks.empty') }}
    </div>
    <div v-else class="max-h-[360px] overflow-auto rounded-xl border border-gray-200 dark:border-dark-700">
      <table class="min-w-[900px] w-full divide-y divide-gray-200 dark:divide-dark-700">
        <thead class="sticky top-0 z-10 bg-gray-50 dark:bg-dark-900">
          <tr>
            <th class="px-4 py-3 text-left text-[11px] font-bold uppercase text-gray-500">{{ t('admin.ops.quotaResetLinks.table.rule') }}</th>
            <th class="px-4 py-3 text-left text-[11px] font-bold uppercase text-gray-500">{{ t('admin.ops.quotaResetLinks.table.binding') }}</th>
            <th class="px-4 py-3 text-left text-[11px] font-bold uppercase text-gray-500">{{ t('admin.ops.quotaResetLinks.table.lastReset') }}</th>
            <th class="px-4 py-3 text-left text-[11px] font-bold uppercase text-gray-500">{{ t('admin.ops.quotaResetLinks.table.status') }}</th>
            <th class="px-4 py-3 text-right text-[11px] font-bold uppercase text-gray-500">{{ t('common.actions') }}</th>
          </tr>
        </thead>
        <tbody class="divide-y divide-gray-100 dark:divide-dark-700">
          <tr v-for="rule in sortedRules" :key="rule.id" class="align-top hover:bg-gray-50 dark:hover:bg-dark-700/40">
            <td class="px-4 py-3">
              <div class="text-xs font-bold text-gray-900 dark:text-white">{{ rule.name }}</div>
              <div v-if="rule.description" class="mt-1 max-w-xs text-[11px] text-gray-500">{{ rule.description }}</div>
            </td>
            <td class="px-4 py-3 text-xs text-gray-700 dark:text-gray-200">
              <div>{{ rule.source_account_name || `#${rule.source_account_id}` }}</div>
              <div v-if="rule.source_identity" class="mt-1 max-w-sm space-y-0.5 text-[11px] text-gray-500 dark:text-gray-400">
                <div>#{{ rule.source_identity.local_account_id }} · {{ rule.source_identity.chatgpt_account_id || t('common.unknown') }}</div>
                <div>{{ rule.source_identity.email || t('common.unknown') }} · {{ rule.source_identity.chatgpt_user_id || t('common.unknown') }}</div>
                <div>{{ rule.source_identity.plan_type || t('common.unknown') }} · {{ rule.source_identity.identity_source }}</div>
                <div>{{ t('admin.ops.quotaResetLinks.lastVerified') }}: {{ rule.source_identity.last_verified_at ? formatDateTime(rule.source_identity.last_verified_at) : t('common.unknown') }}</div>
              </div>
              <div class="mt-1 text-gray-400">→ {{ rule.target_group_name || `#${rule.target_group_id}` }}</div>
            </td>
            <td class="px-4 py-3 text-xs text-gray-700 dark:text-gray-200">
              {{ rule.last_observed_reset_at ? formatDateTime(rule.last_observed_reset_at) : t('admin.ops.quotaResetLinks.baselinePending') }}
            </td>
            <td class="px-4 py-3 text-xs">
              <span :class="rule.enabled ? 'text-emerald-600' : 'text-gray-400'">{{ rule.enabled ? t('common.enabled') : t('common.disabled') }}</span>
              <div v-if="rule.query_failure" class="mt-1 max-w-xs break-words text-[11px] text-red-600">
                {{ rule.query_failure.message }} · {{ diagnosticLabel('stages', rule.query_failure.stage) }} · {{ diagnosticLabel('reasons', rule.query_failure.reason) }}<template v-if="rule.query_failure.request_id"> · {{ rule.query_failure.request_id }}</template>
              </div>
              <div v-if="rule.execution_failure" class="mt-1 max-w-xs break-words text-[11px] text-red-600">
                {{ rule.execution_failure.message }} · {{ diagnosticLabel('stages', rule.execution_failure.stage) }} · {{ diagnosticLabel('reasons', rule.execution_failure.reason) }}<template v-if="rule.execution_failure.request_id"> · {{ rule.execution_failure.request_id }}</template>
              </div>
              <div v-else-if="!rule.query_failure && rule.last_error" class="mt-1 max-w-xs break-words text-[11px] text-red-600">{{ rule.last_error }}</div>
            </td>
            <td class="whitespace-nowrap px-4 py-3 text-right">
              <button class="btn btn-sm btn-secondary" :disabled="checkingId === rule.id" @click="checkNow(rule)">{{ checkingId === rule.id ? t('admin.ops.quotaResetLinks.checking') : t('admin.ops.quotaResetLinks.checkNow') }}</button>
              <button class="ml-2 btn btn-sm btn-secondary" @click="openEdit(rule)">{{ t('common.edit') }}</button>
              <button class="ml-2 btn btn-sm btn-danger" @click="pendingDelete = rule">{{ t('common.delete') }}</button>
            </td>
          </tr>
        </tbody>
      </table>
    </div>

    <div class="mt-6">
      <h4 class="text-xs font-bold text-gray-900 dark:text-white">{{ t('admin.ops.quotaResetLinks.history') }}</h4>
      <div v-if="executions.length === 0" class="mt-3 text-xs text-gray-500">{{ t('admin.ops.quotaResetLinks.historyEmpty') }}</div>
      <div v-else class="mt-3 max-h-[260px] overflow-auto rounded-xl border border-gray-200 dark:border-dark-700">
        <table class="min-w-[760px] w-full divide-y divide-gray-200 text-xs dark:divide-dark-700">
          <tbody class="divide-y divide-gray-100 dark:divide-dark-700">
            <tr v-for="execution in executions" :key="execution.id">
              <td class="px-4 py-3 font-medium text-gray-900 dark:text-white">{{ execution.rule_name || `#${execution.rule_id}` }}</td>
              <td class="px-4 py-3 text-gray-500">{{ formatDateTime(execution.official_reset_at) }}</td>
              <td class="px-4 py-3 text-gray-500">{{ execution.event_source || 'poll' }} · {{ execution.evidence_kind || '' }}</td>
              <td class="px-4 py-3"><span class="rounded-md px-2 py-1 font-medium" :class="statusClass(execution.status)">{{ t(`admin.ops.quotaResetLinks.status.${execution.status}`) }}</span></td>
              <td class="px-4 py-3 text-gray-600 dark:text-gray-300">{{ t('admin.ops.quotaResetLinks.counts', { matched: execution.matched_users, reset: execution.reset_users, skipped: execution.skipped_users }) }}</td>
              <td class="max-w-xs break-words px-4 py-3 text-red-600">{{ execution.error_message || '' }}<template v-if="execution.stage"> · {{ diagnosticLabel('stages', execution.stage) }}</template><template v-if="execution.error_reason"> · {{ diagnosticLabel('reasons', execution.error_reason) }}</template><template v-if="execution.error_request_id"> · {{ execution.error_request_id }}</template></td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>

    <div class="mt-6">
      <h4 class="text-xs font-bold text-gray-900 dark:text-white">{{ t('admin.ops.quotaResetLinks.evidenceHistory') }}</h4>
      <div v-if="resetEvents.length === 0" class="mt-3 text-xs text-gray-500">{{ t('admin.ops.quotaResetLinks.evidenceEmpty') }}</div>
      <div v-else class="mt-3 max-h-[220px] overflow-auto rounded-lg border border-gray-200 dark:border-dark-700">
        <table class="min-w-[760px] w-full divide-y divide-gray-200 text-xs dark:divide-dark-700">
          <tbody class="divide-y divide-gray-100 dark:divide-dark-700">
            <tr v-for="event in resetEvents" :key="event.id">
              <td class="px-4 py-3 font-medium text-gray-900 dark:text-white">{{ event.source_account_name || `#${event.source_account_id}` }}</td>
              <td class="px-4 py-3 text-gray-500">{{ event.event_source }} · {{ event.evidence_kind }}</td>
              <td class="px-4 py-3"><span class="rounded-md px-2 py-1 font-medium" :class="statusClass(event.status)">{{ t(`admin.ops.quotaResetLinks.eventStatus.${event.status}`) }}</span></td>
              <td class="px-4 py-3 text-gray-500">{{ event.reason ? t(`admin.ops.quotaResetLinks.eventReasons.${event.reason}`) : '' }}</td>
              <td class="px-4 py-3 text-gray-500">{{ formatDateTime(event.observed_at) }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>

    <BaseDialog :show="showEditor" :title="editingId ? t('admin.ops.quotaResetLinks.editTitle') : t('admin.ops.quotaResetLinks.createTitle')" width="wide" @close="showEditor = false">
      <div class="grid grid-cols-1 gap-4 md:grid-cols-2">
        <div class="md:col-span-2"><label class="input-label">{{ t('admin.ops.quotaResetLinks.form.name') }}</label><input v-model="draft.name" class="input" /></div>
        <div class="md:col-span-2"><label class="input-label">{{ t('admin.ops.quotaResetLinks.form.description') }}</label><input v-model="draft.description" class="input" /></div>
        <div><label class="input-label">{{ t('admin.ops.quotaResetLinks.form.account') }}</label><Select v-model="draft.source_account_id" searchable :options="accountOptions" /></div>
        <div><label class="input-label">{{ t('admin.ops.quotaResetLinks.form.group') }}</label><Select v-model="draft.target_group_id" searchable :options="groupOptions" /></div>
        <div class="flex items-center justify-between rounded-xl bg-gray-50 px-4 py-3 dark:bg-dark-700 md:col-span-2"><span class="text-xs font-bold text-gray-700 dark:text-gray-200">{{ t('common.enabled') }}</span><Toggle v-model="draft.enabled" /></div>
        <p class="text-xs leading-5 text-gray-500 dark:text-gray-400 md:col-span-2">{{ t('admin.ops.quotaResetLinks.baselineHint') }}</p>
      </div>
      <template #footer><div class="flex justify-end gap-2"><button class="btn btn-secondary" @click="showEditor = false">{{ t('common.cancel') }}</button><button class="btn btn-primary" :disabled="saving || !validDraft" @click="save">{{ saving ? t('common.saving') : t('common.save') }}</button></div></template>
    </BaseDialog>

    <ConfirmDialog :show="pendingDelete != null" :title="t('admin.ops.quotaResetLinks.deleteTitle')" :message="t('admin.ops.quotaResetLinks.deleteMessage')" :confirmText="t('common.delete')" :cancelText="t('common.cancel')" @confirm="confirmDelete" @cancel="pendingDelete = null" />
  </div>
</template>

<template>
  <BaseDialog
    :show="show"
    :title="t('admin.scheduledTests.title')"
    width="wide"
    @close="emit('close')"
  >
    <div class="space-y-4">
      <!-- Add Plan Button -->
      <div class="flex items-center justify-between">
        <p class="text-sm text-gray-500 dark:text-gray-400">
          {{ t('admin.scheduledTests.title') }}
        </p>
        <div class="flex items-center gap-2">
          <button
            data-testid="refresh-scheduled-tests"
            class="rounded-lg p-2 text-gray-500 transition-colors hover:bg-gray-100 hover:text-primary-600 disabled:cursor-not-allowed disabled:opacity-50 dark:text-gray-400 dark:hover:bg-dark-700 dark:hover:text-primary-400"
            :disabled="refreshing"
            :title="t('admin.scheduledTests.refresh')"
            :aria-label="t('admin.scheduledTests.refresh')"
            @click="refreshPanel(true)"
          >
            <Icon
              name="refresh"
              size="sm"
              :class="refreshing ? 'animate-spin' : ''"
              :stroke-width="2"
            />
          </button>
          <button
            @click="showAddForm = !showAddForm"
            class="btn btn-primary flex items-center gap-1.5 text-sm"
          >
            <Icon name="plus" size="sm" :stroke-width="2" />
            {{ t('admin.scheduledTests.addPlan') }}
          </button>
        </div>
      </div>

      <!-- Add Plan Form -->
      <div
        v-if="showAddForm"
        class="rounded-xl border border-primary-200 bg-primary-50/50 p-4 dark:border-primary-800 dark:bg-primary-900/20"
      >
        <div class="mb-3 text-sm font-medium text-gray-700 dark:text-gray-300">
          {{ t('admin.scheduledTests.addPlan') }}
        </div>
        <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
          <div>
            <label class="mb-1 block text-xs font-medium text-gray-600 dark:text-gray-400">
              {{ t('admin.scheduledTests.model') }}
            </label>
            <Select
              v-model="newPlan.model_id"
              data-testid="new-model-id"
              :options="modelOptions"
              :placeholder="t('admin.scheduledTests.model')"
              :searchable="modelOptions.length > 5"
            />
          </div>
          <div>
            <label class="mb-1 flex items-center gap-1 text-xs font-medium text-gray-600 dark:text-gray-400">
              {{ t('admin.scheduledTests.cronExpression') }}
              <HelpTooltip>
                <template #trigger>
                  <span class="inline-flex h-4 w-4 cursor-help items-center justify-center rounded-full border border-gray-400/70 text-[10px] font-semibold text-gray-400 transition-colors hover:border-primary-500 hover:text-primary-600 dark:border-gray-500 dark:text-gray-500 dark:hover:border-primary-400 dark:hover:text-primary-400">
                    ?
                  </span>
                </template>
                <div class="space-y-1.5">
                  <p class="font-medium">{{ t('admin.scheduledTests.cronTooltipTitle') }}</p>
                  <p>{{ t('admin.scheduledTests.cronTooltipMeaning') }}</p>
                  <p>{{ t('admin.scheduledTests.cronTooltipExampleEvery30Min') }}</p>
                  <p>{{ t('admin.scheduledTests.cronTooltipExampleHourly') }}</p>
                  <p>{{ t('admin.scheduledTests.cronTooltipExampleDaily') }}</p>
                  <p>{{ t('admin.scheduledTests.cronTooltipExampleWeekly') }}</p>
                  <p>{{ t('admin.scheduledTests.cronTooltipRange') }}</p>
                </div>
              </HelpTooltip>
            </label>
            <Input
              v-model="newPlan.cron_expression"
              data-testid="new-cron-expression"
              :placeholder="'*/30 * * * *'"
              :hint="t('admin.scheduledTests.cronHelp')"
            />
          </div>
          <div>
            <label class="mb-1 flex items-center gap-1 text-xs font-medium text-gray-600 dark:text-gray-400">
              {{ t('admin.scheduledTests.maxResults') }}
              <HelpTooltip>
                <template #trigger>
                  <span class="inline-flex h-4 w-4 cursor-help items-center justify-center rounded-full border border-gray-400/70 text-[10px] font-semibold text-gray-400 transition-colors hover:border-primary-500 hover:text-primary-600 dark:border-gray-500 dark:text-gray-500 dark:hover:border-primary-400 dark:hover:text-primary-400">
                    ?
                  </span>
                </template>
                <div class="space-y-1.5">
                  <p class="font-medium">{{ t('admin.scheduledTests.maxResultsTooltipTitle') }}</p>
                  <p>{{ t('admin.scheduledTests.maxResultsTooltipMeaning') }}</p>
                  <p>{{ t('admin.scheduledTests.maxResultsTooltipBody') }}</p>
                  <p>{{ t('admin.scheduledTests.maxResultsTooltipExample') }}</p>
                  <p>{{ t('admin.scheduledTests.maxResultsTooltipRange') }}</p>
                </div>
              </HelpTooltip>
            </label>
            <Input
              v-model="newPlan.max_results"
              type="number"
              placeholder="100"
            />
          </div>
          <div class="flex items-end">
            <label class="flex items-center gap-2 text-sm text-gray-700 dark:text-gray-300">
              <Toggle v-model="newPlan.enabled" />
              {{ t('admin.scheduledTests.enabled') }}
            </label>
          </div>
          <div class="flex items-end">
            <div>
              <label class="flex items-center gap-2 text-sm text-gray-700 dark:text-gray-300">
                <Toggle v-model="newPlan.auto_recover" />
                {{ t('admin.scheduledTests.autoRecover') }}
              </label>
              <p class="mt-0.5 text-xs text-gray-400 dark:text-gray-500">
                {{ t('admin.scheduledTests.autoRecoverHelp') }}
              </p>
            </div>
          </div>
          <div>
            <label class="mb-1 block text-xs font-medium text-gray-600 dark:text-gray-400">
              {{ t('admin.scheduledTests.timeoutProtectionMode') }}
            </label>
            <Select
              v-model="newPlan.timeout_protection_mode"
              data-testid="new-timeout-protection-mode"
              :options="timeoutProtectionModeOptions"
            />
          </div>
          <div>
            <label class="mb-1 block text-xs font-medium text-gray-600 dark:text-gray-400">
              {{ t('admin.scheduledTests.timeoutSeconds') }}
            </label>
            <Input
              v-model="newPlan.timeout_seconds"
              data-testid="new-timeout-seconds"
              type="number"
              placeholder="60"
            />
          </div>
          <div>
            <label class="mb-1 block text-xs font-medium text-gray-600 dark:text-gray-400">
              {{ t('admin.scheduledTests.consecutiveTimeoutThreshold') }}
            </label>
            <Input
              v-model="newPlan.consecutive_timeout_threshold"
              data-testid="new-consecutive-timeout-threshold"
              type="number"
              placeholder="3"
            />
          </div>
          <div>
            <label class="mb-1 block text-xs font-medium text-gray-600 dark:text-gray-400">
              {{ t('admin.scheduledTests.retryDelaysSeconds') }}
            </label>
            <Input
              v-model="newPlan.retry_delays_seconds"
              data-testid="new-retry-delays-seconds"
              placeholder="10, 20"
              :hint="t('admin.scheduledTests.retryDelaysHelp')"
            />
          </div>
        </div>
        <div class="mt-3 flex justify-end gap-2">
          <button
            @click="showAddForm = false; resetNewPlan()"
            class="rounded-lg bg-gray-100 px-3 py-1.5 text-sm font-medium text-gray-700 transition-colors hover:bg-gray-200 dark:bg-dark-600 dark:text-gray-300 dark:hover:bg-dark-500"
          >
            {{ t('common.cancel') }}
          </button>
          <button
            @click="handleCreate"
            :disabled="!newPlan.model_id || !newPlan.cron_expression || creating"
            class="flex items-center gap-1.5 rounded-lg bg-primary-500 px-3 py-1.5 text-sm font-medium text-white transition-colors hover:bg-primary-600 disabled:cursor-not-allowed disabled:opacity-50"
          >
            <Icon v-if="creating" name="refresh" size="sm" class="animate-spin" :stroke-width="2" />
            {{ t('common.save') }}
          </button>
        </div>
      </div>

      <!-- Loading State -->
      <div v-if="loading" class="flex items-center justify-center py-8">
        <Icon name="refresh" size="md" class="animate-spin text-gray-400" :stroke-width="2" />
        <span class="ml-2 text-sm text-gray-500">{{ t('common.loading') }}...</span>
      </div>

      <!-- Empty State -->
      <div
        v-else-if="plans.length === 0"
        class="rounded-xl border border-dashed border-gray-300 py-10 text-center dark:border-dark-600"
      >
        <Icon name="calendar" size="lg" class="mx-auto mb-2 text-gray-400" :stroke-width="1.5" />
        <p class="text-sm text-gray-500 dark:text-gray-400">
          {{ t('admin.scheduledTests.noPlans') }}
        </p>
      </div>

      <!-- Plans List -->
      <div v-else class="space-y-3">
        <div
          v-for="plan in plans"
          :key="plan.id"
          class="rounded-xl border border-gray-200 bg-white transition-all dark:border-dark-600 dark:bg-dark-800"
        >
          <!-- Plan Header -->
          <div
            class="flex cursor-pointer items-center justify-between px-4 py-3"
            @click="toggleExpand(plan.id)"
          >
            <div class="flex flex-1 items-center gap-4">
              <!-- Model -->
              <div class="min-w-0">
                <div class="text-sm font-medium text-gray-900 dark:text-gray-100">
                  {{ plan.model_id }}
                </div>
                <div class="mt-0.5 font-mono text-xs text-gray-500 dark:text-gray-400">
                  {{ plan.cron_expression }}
                </div>
              </div>

              <!-- Enabled Toggle -->
              <div class="flex items-center gap-1.5" @click.stop>
                <Toggle
                  :key="`${plan.id}:${toggleRenderVersion}`"
                  :model-value="plan.enabled"
                  @update:model-value="(val: boolean) => handleToggleEnabled(plan, val)"
                />
                <span class="text-xs text-gray-500 dark:text-gray-400">
                  {{ plan.enabled ? t('admin.scheduledTests.enabled') : '' }}
                </span>
              </div>

              <!-- Auto Recover Badge -->
              <span
                v-if="plan.auto_recover"
                class="inline-flex items-center rounded-full bg-emerald-100 px-2 py-0.5 text-xs font-medium text-emerald-700 dark:bg-emerald-500/20 dark:text-emerald-400"
              >
                {{ t('admin.scheduledTests.autoRecover') }}
              </span>

              <span class="text-xs text-gray-500 dark:text-gray-400">
                {{ t('admin.scheduledTests.timeoutProtectionMode') }}:
                {{ t(`admin.scheduledTests.timeoutProtectionModes.${plan.timeout_protection_mode}`) }}
              </span>

              <template
                v-if="plan.effective_timeout_protection_mode && plan.effective_timeout_protection_mode !== plan.timeout_protection_mode"
              >
                <span class="text-xs font-medium text-amber-700 dark:text-amber-400">
                  {{ t('admin.scheduledTests.effectiveTimeoutProtectionMode') }}:
                  {{ t(`admin.scheduledTests.timeoutProtectionModes.${plan.effective_timeout_protection_mode}`) }}
                </span>
                <span
                  v-if="plan.timeout_protection_override_reason"
                  class="text-xs text-amber-700 dark:text-amber-400"
                >
                  {{ t('admin.scheduledTests.timeoutProtectionOverrideReason') }}:
                  {{ formatTimeoutProtectionOverrideReason(plan.timeout_protection_override_reason) }}
                </span>
              </template>

              <span class="text-xs text-gray-500 dark:text-gray-400">
                {{ t('admin.scheduledTests.consecutiveTimeoutCount') }}:
                {{ plan.consecutive_timeout_count }}
              </span>

              <span
                :class="[
                  'inline-flex items-center rounded-full px-2 py-0.5 text-xs font-medium',
                  plan.owns_inactive_account
                    ? 'bg-amber-100 text-amber-700 dark:bg-amber-500/20 dark:text-amber-400'
                    : 'bg-gray-100 text-gray-600 dark:bg-dark-700 dark:text-gray-400'
                ]"
              >
                {{ t('admin.scheduledTests.ownsInactiveAccount') }}:
                {{ plan.owns_inactive_account ? t('common.yes') : t('common.no') }}
              </span>
            </div>

            <div class="flex items-center gap-3">
              <!-- Last Run -->
              <div v-if="plan.last_run_at" class="hidden text-right text-xs text-gray-500 dark:text-gray-400 sm:block">
                <div>{{ t('admin.scheduledTests.lastRun') }}</div>
                <div>{{ formatDateTime(plan.last_run_at) }}</div>
              </div>

              <!-- Next Run -->
              <div v-if="plan.next_run_at" class="hidden text-right text-xs text-gray-500 dark:text-gray-400 sm:block">
                <div>{{ t('admin.scheduledTests.nextRun') }}</div>
                <div>{{ formatDateTime(plan.next_run_at) }}</div>
              </div>

              <!-- Actions -->
              <div class="flex items-center gap-1" @click.stop>
                <button
                  @click="startEdit(plan)"
                  class="rounded-lg p-1.5 text-gray-400 transition-colors hover:bg-blue-50 hover:text-blue-500 dark:hover:bg-blue-900/20"
                  :title="t('admin.scheduledTests.editPlan')"
                >
                  <Icon name="edit" size="sm" :stroke-width="2" />
                </button>
                <button
                  @click="confirmDeletePlan(plan)"
                  class="rounded-lg p-1.5 text-gray-400 transition-colors hover:bg-red-50 hover:text-red-500 dark:hover:bg-red-900/20"
                  :title="t('admin.scheduledTests.deletePlan')"
                >
                  <Icon name="trash" size="sm" :stroke-width="2" />
                </button>
              </div>

              <!-- Expand indicator -->
              <Icon
                name="chevronDown"
                size="sm"
                :class="[
                  'text-gray-400 transition-transform duration-200',
                  expandedPlanId === plan.id ? 'rotate-180' : ''
                ]"
              />
            </div>
          </div>

          <!-- Edit Form -->
          <div
            v-if="editingPlanId === plan.id"
            data-testid="edit-plan-form"
            class="border-t border-blue-100 bg-blue-50/50 px-4 py-3 dark:border-blue-900 dark:bg-blue-900/10"
            @click.stop
          >
            <div class="mb-2 text-xs font-medium text-gray-600 dark:text-gray-400">
              {{ t('admin.scheduledTests.editPlan') }}
            </div>
            <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
              <div>
                <label class="mb-1 block text-xs font-medium text-gray-600 dark:text-gray-400">
                  {{ t('admin.scheduledTests.model') }}
                </label>
                <Select
                  v-model="editForm.model_id"
                  :options="modelOptions"
                  :placeholder="t('admin.scheduledTests.model')"
                  :searchable="modelOptions.length > 5"
                />
              </div>
              <div>
                <label class="mb-1 flex items-center gap-1 text-xs font-medium text-gray-600 dark:text-gray-400">
                  {{ t('admin.scheduledTests.cronExpression') }}
                  <HelpTooltip>
                    <template #trigger>
                      <span class="inline-flex h-4 w-4 cursor-help items-center justify-center rounded-full border border-gray-400/70 text-[10px] font-semibold text-gray-400 transition-colors hover:border-primary-500 hover:text-primary-600 dark:border-gray-500 dark:text-gray-500 dark:hover:border-primary-400 dark:hover:text-primary-400">
                        ?
                      </span>
                    </template>
                    <div class="space-y-1.5">
                      <p class="font-medium">{{ t('admin.scheduledTests.cronTooltipTitle') }}</p>
                      <p>{{ t('admin.scheduledTests.cronTooltipMeaning') }}</p>
                      <p>{{ t('admin.scheduledTests.cronTooltipExampleEvery30Min') }}</p>
                      <p>{{ t('admin.scheduledTests.cronTooltipExampleHourly') }}</p>
                      <p>{{ t('admin.scheduledTests.cronTooltipExampleDaily') }}</p>
                      <p>{{ t('admin.scheduledTests.cronTooltipExampleWeekly') }}</p>
                      <p>{{ t('admin.scheduledTests.cronTooltipRange') }}</p>
                    </div>
                  </HelpTooltip>
                </label>
                <Input
                  v-model="editForm.cron_expression"
                  :placeholder="'*/30 * * * *'"
                  :hint="t('admin.scheduledTests.cronHelp')"
                />
              </div>
              <div>
                <label class="mb-1 flex items-center gap-1 text-xs font-medium text-gray-600 dark:text-gray-400">
                  {{ t('admin.scheduledTests.maxResults') }}
                  <HelpTooltip>
                    <template #trigger>
                      <span class="inline-flex h-4 w-4 cursor-help items-center justify-center rounded-full border border-gray-400/70 text-[10px] font-semibold text-gray-400 transition-colors hover:border-primary-500 hover:text-primary-600 dark:border-gray-500 dark:text-gray-500 dark:hover:border-primary-400 dark:hover:text-primary-400">
                        ?
                      </span>
                    </template>
                    <div class="space-y-1.5">
                      <p class="font-medium">{{ t('admin.scheduledTests.maxResultsTooltipTitle') }}</p>
                      <p>{{ t('admin.scheduledTests.maxResultsTooltipMeaning') }}</p>
                      <p>{{ t('admin.scheduledTests.maxResultsTooltipBody') }}</p>
                      <p>{{ t('admin.scheduledTests.maxResultsTooltipExample') }}</p>
                      <p>{{ t('admin.scheduledTests.maxResultsTooltipRange') }}</p>
                    </div>
                  </HelpTooltip>
                </label>
                <Input
                  v-model="editForm.max_results"
                  type="number"
                  placeholder="100"
                />
              </div>
              <div class="flex items-end">
                <label class="flex items-center gap-2 text-sm text-gray-700 dark:text-gray-300">
                  <Toggle v-model="editForm.enabled" />
                  {{ t('admin.scheduledTests.enabled') }}
                </label>
              </div>
              <div class="flex items-end">
                <div>
                  <label class="flex items-center gap-2 text-sm text-gray-700 dark:text-gray-300">
                    <Toggle v-model="editForm.auto_recover" />
                    {{ t('admin.scheduledTests.autoRecover') }}
                  </label>
                  <p class="mt-0.5 text-xs text-gray-400 dark:text-gray-500">
                    {{ t('admin.scheduledTests.autoRecoverHelp') }}
                  </p>
                </div>
              </div>
              <div>
                <label class="mb-1 block text-xs font-medium text-gray-600 dark:text-gray-400">
                  {{ t('admin.scheduledTests.timeoutProtectionMode') }}
                </label>
                <Select
                  v-model="editForm.timeout_protection_mode"
                  data-testid="edit-timeout-protection-mode"
                  :options="timeoutProtectionModeOptions"
                />
              </div>
              <div>
                <label class="mb-1 block text-xs font-medium text-gray-600 dark:text-gray-400">
                  {{ t('admin.scheduledTests.timeoutSeconds') }}
                </label>
                <Input
                  v-model="editForm.timeout_seconds"
                  data-testid="edit-timeout-seconds"
                  type="number"
                  placeholder="60"
                />
              </div>
              <div>
                <label class="mb-1 block text-xs font-medium text-gray-600 dark:text-gray-400">
                  {{ t('admin.scheduledTests.consecutiveTimeoutThreshold') }}
                </label>
                <Input
                  v-model="editForm.consecutive_timeout_threshold"
                  data-testid="edit-consecutive-timeout-threshold"
                  type="number"
                  placeholder="3"
                />
              </div>
              <div>
                <label class="mb-1 block text-xs font-medium text-gray-600 dark:text-gray-400">
                  {{ t('admin.scheduledTests.retryDelaysSeconds') }}
                </label>
                <Input
                  v-model="editForm.retry_delays_seconds"
                  data-testid="edit-retry-delays-seconds"
                  placeholder="10, 20"
                  :hint="t('admin.scheduledTests.retryDelaysHelp')"
                />
              </div>
            </div>
            <div class="mt-3 flex justify-end gap-2">
              <button
                @click="cancelEdit"
                class="rounded-lg bg-gray-100 px-3 py-1.5 text-sm font-medium text-gray-700 transition-colors hover:bg-gray-200 dark:bg-dark-600 dark:text-gray-300 dark:hover:bg-dark-500"
              >
                {{ t('common.cancel') }}
              </button>
              <button
                @click="handleEdit"
                :disabled="!editForm.model_id || !editForm.cron_expression || updating"
                class="flex items-center gap-1.5 rounded-lg bg-primary-500 px-3 py-1.5 text-sm font-medium text-white transition-colors hover:bg-primary-600 disabled:cursor-not-allowed disabled:opacity-50"
              >
                <Icon v-if="updating" name="refresh" size="sm" class="animate-spin" :stroke-width="2" />
                {{ t('common.save') }}
              </button>
            </div>
          </div>

          <!-- Expanded Results Section -->
          <div
            v-if="expandedPlanId === plan.id"
            class="border-t border-gray-100 px-4 py-3 dark:border-dark-700"
          >
            <div class="mb-2 text-xs font-medium text-gray-600 dark:text-gray-400">
              {{ t('admin.scheduledTests.results') }}
            </div>

            <!-- Results Loading -->
            <div v-if="loadingResults" class="flex items-center justify-center py-4">
              <Icon name="refresh" size="sm" class="animate-spin text-gray-400" :stroke-width="2" />
              <span class="ml-2 text-xs text-gray-500">{{ t('common.loading') }}...</span>
            </div>

            <!-- No Results -->
            <div
              v-else-if="results.length === 0"
              class="py-4 text-center text-xs text-gray-500 dark:text-gray-400"
            >
              {{ t('admin.scheduledTests.noResults') }}
            </div>

            <!-- Results List -->
            <div v-else class="max-h-64 space-y-2 overflow-y-auto">
              <div
                v-for="result in results"
                :key="result.id"
                class="rounded-lg border border-gray-100 bg-gray-50 p-3 dark:border-dark-700 dark:bg-dark-900"
              >
                <div class="flex items-center justify-between">
                  <div class="flex items-center gap-2">
                    <!-- Status Badge -->
                    <span
                      :class="[
                        'inline-flex items-center rounded-full px-2 py-0.5 text-xs font-medium',
                        result.status === 'success'
                          ? 'bg-green-100 text-green-700 dark:bg-green-500/20 dark:text-green-400'
                          : result.status === 'running'
                            ? 'bg-blue-100 text-blue-700 dark:bg-blue-500/20 dark:text-blue-400'
                            : 'bg-red-100 text-red-700 dark:bg-red-500/20 dark:text-red-400'
                      ]"
                    >
                      {{
                        result.status === 'success'
                          ? t('admin.scheduledTests.success')
                          : result.status === 'running'
                            ? t('admin.scheduledTests.running')
                            : t('admin.scheduledTests.failed')
                      }}
                    </span>

                    <!-- Latency -->
                    <span v-if="result.latency_ms > 0" class="text-xs text-gray-500 dark:text-gray-400">
                      {{ result.latency_ms }}ms
                    </span>
                  </div>

                  <!-- Started At -->
                  <span class="text-xs text-gray-400">
                    {{ formatDateTime(result.started_at) }}
                  </span>
                </div>

                <dl class="mt-2 grid grid-cols-1 gap-x-4 gap-y-1 text-xs text-gray-500 dark:text-gray-400 sm:grid-cols-2">
                  <div v-if="result.run_mode">
                    <dt class="inline font-medium">{{ t('admin.scheduledTests.runMode') }}:</dt>
                    <dd class="inline"> {{ formatScheduledTestEnum('runModes', result.run_mode) }}</dd>
                  </div>
                  <div v-if="result.classification">
                    <dt class="inline font-medium">{{ t('admin.scheduledTests.classification') }}:</dt>
                    <dd class="inline"> {{ formatScheduledTestEnum('classifications', result.classification) }}</dd>
                  </div>
                  <div v-if="result.protection_action">
                    <dt class="inline font-medium">{{ t('admin.scheduledTests.protectionAction') }}:</dt>
                    <dd class="inline"> {{ formatScheduledTestEnum('protectionActions', result.protection_action) }}</dd>
                  </div>
                  <div v-if="result.blocked_reason">
                    <dt class="inline font-medium">{{ t('admin.scheduledTests.blockedReason') }}:</dt>
                    <dd class="inline break-all"> {{ formatScheduledTestEnum('blockedReasons', result.blocked_reason) }}</dd>
                  </div>
                </dl>

                <!-- Response / Error (collapsible) -->
                <div v-if="result.error_message" class="mt-2">
                  <div
                    class="cursor-pointer text-xs font-medium text-red-600 dark:text-red-400"
                    @click="toggleResultDetail(result.id)"
                  >
                    {{ t('admin.scheduledTests.errorMessage') }}
                    <Icon
                      name="chevronDown"
                      size="sm"
                      :class="[
                        'inline transition-transform duration-200',
                        expandedResultIds.has(result.id) ? 'rotate-180' : ''
                      ]"
                    />
                  </div>
                  <pre
                    v-if="expandedResultIds.has(result.id)"
                    class="mt-1 max-h-32 overflow-auto whitespace-pre-wrap rounded bg-red-50 p-2 text-xs text-red-700 dark:bg-red-900/20 dark:text-red-300"
                  >{{ result.error_message }}</pre>
                </div>
                <div v-else-if="result.response_text" class="mt-2">
                  <div
                    class="cursor-pointer text-xs font-medium text-gray-600 dark:text-gray-400"
                    @click="toggleResultDetail(result.id)"
                  >
                    {{ t('admin.scheduledTests.responseText') }}
                    <Icon
                      name="chevronDown"
                      size="sm"
                      :class="[
                        'inline transition-transform duration-200',
                        expandedResultIds.has(result.id) ? 'rotate-180' : ''
                      ]"
                    />
                  </div>
                  <pre
                    v-if="expandedResultIds.has(result.id)"
                    class="mt-1 max-h-32 overflow-auto whitespace-pre-wrap rounded bg-gray-100 p-2 text-xs text-gray-700 dark:bg-dark-800 dark:text-gray-300"
                  >{{ result.response_text }}</pre>
                </div>
              </div>
            </div>
          </div>
        </div>
      </div>
    </div>

    <ConfirmDialog
      data-testid="confirm-restore-ownership"
      :show="pendingOwnershipUpdate !== null"
      :title="t('admin.scheduledTests.restoreOwnershipTitle')"
      :message="t('admin.scheduledTests.confirmRestoreOwnership')"
      :confirm-text="t('admin.scheduledTests.restoreAndDisable')"
      :cancel-text="t('common.cancel')"
      :danger="true"
      @confirm="confirmOwnershipUpdate"
      @cancel="cancelOwnershipUpdate"
    />

    <!-- Delete Confirmation -->
    <ConfirmDialog
      data-testid="confirm-delete"
      :show="showDeleteConfirm"
      :title="t('admin.scheduledTests.deletePlan')"
      :message="deleteConfirmationMessage"
      :confirm-text="t('common.delete')"
      :cancel-text="t('common.cancel')"
      :danger="true"
      @confirm="handleDelete"
      @cancel="cancelDelete"
    />
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref, reactive, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import HelpTooltip from '@/components/common/HelpTooltip.vue'
import Select, { type SelectOption } from '@/components/common/Select.vue'
import Input from '@/components/common/Input.vue'
import Toggle from '@/components/common/Toggle.vue'
import { Icon } from '@/components/icons'
import { adminAPI } from '@/api/admin'
import { useAppStore } from '@/stores/app'
import { formatDateTime } from '@/utils/format'
import type {
  ScheduledTestPlan,
  ScheduledTestResult,
  TimeoutProtectionMode,
  UpdateScheduledTestPlanRequest
} from '@/types'

const { t } = useI18n()
const appStore = useAppStore()

const props = defineProps<{
  show: boolean
  accountId: number | null
  modelOptions: SelectOption[]
}>()

const emit = defineEmits<{
  (e: 'close'): void
}>()

// State
const loading = ref(false)
const refreshing = ref(false)
const creating = ref(false)
const loadingResults = ref(false)
let requestGeneration = 0
let refreshInFlightGeneration: number | null = null
let pollingTimer: ReturnType<typeof setInterval> | null = null
const plans = ref<ScheduledTestPlan[]>([])
const results = ref<ScheduledTestResult[]>([])
const toggleRenderVersion = ref(0)
const expandedPlanId = ref<number | null>(null)
const expandedResultIds = reactive(new Set<number>())
const showAddForm = ref(false)
const showDeleteConfirm = ref(false)
const deletingPlan = ref<ScheduledTestPlan | null>(null)
const deleteRequiresOwnershipRestore = ref(false)
const pendingOwnershipUpdate = ref<{
  plan: ScheduledTestPlan
  request: UpdateScheduledTestPlanRequest
  closeEditOnSuccess: boolean
} | null>(null)
const editingPlanId = ref<number | null>(null)
const updating = ref(false)
const timeoutProtectionModeOptions = computed<SelectOption[]>(() => [
  { value: 'off', label: t('admin.scheduledTests.timeoutProtectionModes.off') },
  { value: 'shadow', label: t('admin.scheduledTests.timeoutProtectionModes.shadow') },
  { value: 'enforce', label: t('admin.scheduledTests.timeoutProtectionModes.enforce') }
])
const deleteConfirmationMessage = computed(() =>
  deleteRequiresOwnershipRestore.value
    ? t('admin.scheduledTests.confirmDeleteWithOwnership')
    : t('admin.scheduledTests.confirmDelete')
)

const editForm = reactive({
  model_id: '' as string,
  cron_expression: '' as string,
  max_results: '100' as string,
  enabled: true,
  auto_recover: false,
  timeout_protection_mode: 'off' as TimeoutProtectionMode,
  timeout_seconds: '60' as string,
  consecutive_timeout_threshold: '3' as string,
  retry_delays_seconds: '10, 20' as string
})

const newPlan = reactive({
  model_id: '' as string,
  cron_expression: '' as string,
  max_results: '100' as string,
  enabled: true,
  auto_recover: false,
  timeout_protection_mode: 'off' as TimeoutProtectionMode,
  timeout_seconds: '60' as string,
  consecutive_timeout_threshold: '3' as string,
  retry_delays_seconds: '10, 20' as string
})

const resetNewPlan = () => {
  newPlan.model_id = ''
  newPlan.cron_expression = ''
  newPlan.max_results = '100'
  newPlan.enabled = true
  newPlan.auto_recover = false
  newPlan.timeout_protection_mode = 'off'
  newPlan.timeout_seconds = '60'
  newPlan.consecutive_timeout_threshold = '3'
  newPlan.retry_delays_seconds = '10, 20'
}

const parseOptionalInteger = (
  value: string,
  min: number,
  max: number
): number | undefined | null => {
  const trimmed = value.trim()
  if (trimmed === '') return undefined

  const parsed = Number(trimmed)
  if (!Number.isInteger(parsed) || parsed < min || parsed > max) return null
  return parsed
}

const parseTimeoutNumbersOrShowError = (
  timeoutSecondsValue: string,
  thresholdValue: string
): { timeoutSeconds?: number; threshold?: number } | null => {
  const timeoutSeconds = parseOptionalInteger(timeoutSecondsValue, 0, 600)
  const threshold = parseOptionalInteger(thresholdValue, 0, 100)
  if (timeoutSeconds === null || threshold === null) {
    appStore.showError(t('admin.scheduledTests.invalidTimeoutProtectionNumbers'))
    return null
  }
  return { timeoutSeconds, threshold }
}

const formatTimeoutProtectionOverrideReason = (reason: string): string => {
  const knownReasons = ['force_shadow', 'kill_switch']
  return knownReasons.includes(reason)
    ? t(`admin.scheduledTests.timeoutProtectionOverrideReasons.${reason}`)
    : reason
}

const scheduledTestEnumValues = {
  runModes: ['normal', 'recovery'],
  classifications: ['success', 'timeout', 'failure'],
  protectionActions: ['none', 'would_inactivate', 'inactivated', 'recovered', 'blocked'],
  blockedReasons: ['kill_switch', 'platform_circuit_open', 'disable_budget_exhausted', 'account_already_owned', 'manual_override']
} as const

const formatScheduledTestEnum = (
  group: keyof typeof scheduledTestEnumValues,
  value: string
): string => {
  return scheduledTestEnumValues[group].some((knownValue) => knownValue === value)
    ? t(`admin.scheduledTests.${group}.${value}`)
    : value
}

const parseRetryDelays = (value: string): number[] | null => {
  if (value.trim() === '') return []

  const parts = value.split(',').map((delay) => delay.trim())
  if (parts.length > 5 || parts.some((delay) => delay === '')) return null

  const delays = parts.map(Number)
  if (delays.some((delay) => !Number.isInteger(delay) || delay < 0 || delay > 300)) {
    return null
  }
  return delays
}

const parseRetryDelaysOrShowError = (value: string): number[] | null => {
  const delays = parseRetryDelays(value)
  if (delays === null) {
    appStore.showError(t('admin.scheduledTests.invalidRetryDelays'))
  }
  return delays
}

const stopPolling = () => {
  if (pollingTimer) {
    clearInterval(pollingTimer)
    pollingTimer = null
  }
}

const startPolling = () => {
  stopPolling()
  pollingTimer = setInterval(() => {
    void refreshPanel(false)
  }, 15_000)
}

const resetPanelState = () => {
  plans.value = []
  results.value = []
  expandedPlanId.value = null
  expandedResultIds.clear()
  showAddForm.value = false
  showDeleteConfirm.value = false
  deletingPlan.value = null
  deleteRequiresOwnershipRestore.value = false
  pendingOwnershipUpdate.value = null
  editingPlanId.value = null
}

const requestIsCurrent = (generation: number, accountId: number) =>
  requestGeneration === generation && props.show && props.accountId === accountId

// Reload and invalidate pending responses when the dialog or account changes.
watch(
  [() => props.show, () => props.accountId],
  async ([visible, accountId]) => {
    requestGeneration += 1
    refreshInFlightGeneration = null
    stopPolling()
    resetPanelState()
    if (visible && accountId) {
      await refreshPanel(false, true)
      if (props.show && props.accountId === accountId) startPolling()
    }
  }
)

onBeforeUnmount(() => {
  requestGeneration += 1
  stopPolling()
})

const loadPlans = async (
  showLoading = true,
  generation = requestGeneration,
  accountId = props.accountId
) => {
  if (!accountId) return
  if (showLoading && requestIsCurrent(generation, accountId)) loading.value = true
  try {
    const loadedPlans = await adminAPI.scheduledTests.listByAccount(accountId)
    if (requestIsCurrent(generation, accountId)) plans.value = loadedPlans
  } catch (error: any) {
    if (requestIsCurrent(generation, accountId)) {
      appStore.showError(error?.message || 'Failed to load plans')
    }
  } finally {
    if (showLoading && requestIsCurrent(generation, accountId)) loading.value = false
  }
}

const loadResults = async (
  planId: number,
  showLoading = true,
  generation = requestGeneration,
  accountId = props.accountId
) => {
  if (!accountId) return
  if (showLoading && requestIsCurrent(generation, accountId)) loadingResults.value = true
  try {
    const loadedResults = await adminAPI.scheduledTests.listResults(planId, 20)
    if (requestIsCurrent(generation, accountId) && expandedPlanId.value === planId) {
      results.value = loadedResults
    }
  } catch (error: any) {
    if (requestIsCurrent(generation, accountId) && expandedPlanId.value === planId) {
      appStore.showError(error?.message || 'Failed to load results')
      results.value = []
    }
  } finally {
    if (showLoading && requestIsCurrent(generation, accountId)) loadingResults.value = false
  }
}

const refreshPanel = async (manual = false, showLoading = false) => {
  const accountId = props.accountId
  const generation = requestGeneration
  if (!props.show || !accountId || refreshInFlightGeneration === generation) return
  refreshInFlightGeneration = generation
  if (manual) refreshing.value = true
  try {
    await loadPlans(showLoading, generation, accountId)
    const planId = expandedPlanId.value
    if (planId && requestIsCurrent(generation, accountId)) {
      await loadResults(planId, false, generation, accountId)
    }
  } finally {
    if (refreshInFlightGeneration === generation) refreshInFlightGeneration = null
    if (manual && requestIsCurrent(generation, accountId)) refreshing.value = false
  }
}

const handleCreate = async () => {
  if (!props.accountId || !newPlan.model_id || !newPlan.cron_expression) return
  const retryDelays = parseRetryDelaysOrShowError(newPlan.retry_delays_seconds)
  if (retryDelays === null) return
  const timeoutNumbers = parseTimeoutNumbersOrShowError(
    newPlan.timeout_seconds,
    newPlan.consecutive_timeout_threshold
  )
  if (timeoutNumbers === null) return
  creating.value = true
  try {
    const maxResults = Number(newPlan.max_results) || 100
    await adminAPI.scheduledTests.create({
      account_id: props.accountId,
      model_id: newPlan.model_id,
      cron_expression: newPlan.cron_expression,
      enabled: newPlan.enabled,
      max_results: maxResults,
      auto_recover: newPlan.auto_recover,
      timeout_protection_mode: newPlan.timeout_protection_mode,
      ...(timeoutNumbers.timeoutSeconds !== undefined && {
        timeout_seconds: timeoutNumbers.timeoutSeconds
      }),
      ...(timeoutNumbers.threshold !== undefined && {
        consecutive_timeout_threshold: timeoutNumbers.threshold
      }),
      retry_delays_seconds: retryDelays
    })
    appStore.showSuccess(t('admin.scheduledTests.createSuccess'))
    showAddForm.value = false
    resetNewPlan()
    await loadPlans()
  } catch (error: any) {
    appStore.showError(error?.message || 'Failed to create plan')
  } finally {
    creating.value = false
  }
}

const isConflict = (error: any) => error?.status === 409 || error?.response?.status === 409

const applyUpdatedPlan = (updated: ScheduledTestPlan) => {
  const index = plans.value.findIndex((plan) => plan.id === updated.id)
  if (index !== -1) plans.value[index] = updated
}

const executePlanUpdate = async (
  plan: ScheduledTestPlan,
  request: UpdateScheduledTestPlanRequest,
  closeEditOnSuccess: boolean,
  restoreOwnedAccount = false
) => {
  try {
    const updated = restoreOwnedAccount
      ? await adminAPI.scheduledTests.update(plan.id, request, { restore_owned_account: true })
      : await adminAPI.scheduledTests.update(plan.id, request)
    applyUpdatedPlan(updated)
    appStore.showSuccess(t('admin.scheduledTests.updateSuccess'))
    if (closeEditOnSuccess) editingPlanId.value = null
  } catch (error: any) {
    if (!restoreOwnedAccount && request.enabled === false && isConflict(error)) {
      pendingOwnershipUpdate.value = { plan, request, closeEditOnSuccess }
      return
    }
    appStore.showError(error?.message || 'Failed to update plan')
  } finally {
    toggleRenderVersion.value += 1
  }
}

const requestPlanUpdate = async (
  plan: ScheduledTestPlan,
  request: UpdateScheduledTestPlanRequest,
  closeEditOnSuccess: boolean
) => {
  if (request.enabled === false && plan.owns_inactive_account) {
    pendingOwnershipUpdate.value = { plan, request, closeEditOnSuccess }
    toggleRenderVersion.value += 1
    return
  }
  await executePlanUpdate(plan, request, closeEditOnSuccess)
}

const confirmOwnershipUpdate = async () => {
  const pending = pendingOwnershipUpdate.value
  if (!pending) return
  pendingOwnershipUpdate.value = null
  await executePlanUpdate(pending.plan, pending.request, pending.closeEditOnSuccess, true)
}

const cancelOwnershipUpdate = () => {
  pendingOwnershipUpdate.value = null
  toggleRenderVersion.value += 1
}

const handleToggleEnabled = async (plan: ScheduledTestPlan, enabled: boolean) => {
  await requestPlanUpdate(plan, { enabled }, false)
}

const startEdit = (plan: ScheduledTestPlan) => {
  editingPlanId.value = plan.id
  editForm.model_id = plan.model_id
  editForm.cron_expression = plan.cron_expression
  editForm.max_results = String(plan.max_results)
  editForm.enabled = plan.enabled
  editForm.auto_recover = plan.auto_recover
  editForm.timeout_protection_mode = plan.timeout_protection_mode || 'off'
  editForm.timeout_seconds = String(plan.timeout_seconds ?? 60)
  editForm.consecutive_timeout_threshold = String(plan.consecutive_timeout_threshold ?? 3)
  editForm.retry_delays_seconds = (plan.retry_delays_seconds ?? [10, 20]).join(', ')
}

const cancelEdit = () => {
  editingPlanId.value = null
}

const handleEdit = async () => {
  if (!editingPlanId.value || !editForm.model_id || !editForm.cron_expression) return
  const plan = plans.value.find((candidate) => candidate.id === editingPlanId.value)
  if (!plan) return
  const retryDelays = parseRetryDelaysOrShowError(editForm.retry_delays_seconds)
  if (retryDelays === null) return
  const timeoutNumbers = parseTimeoutNumbersOrShowError(
    editForm.timeout_seconds,
    editForm.consecutive_timeout_threshold
  )
  if (timeoutNumbers === null) return
  updating.value = true
  try {
    await requestPlanUpdate(plan, {
      model_id: editForm.model_id,
      cron_expression: editForm.cron_expression,
      max_results: Number(editForm.max_results) || 100,
      enabled: editForm.enabled,
      auto_recover: editForm.auto_recover,
      timeout_protection_mode: editForm.timeout_protection_mode,
      ...(timeoutNumbers.timeoutSeconds !== undefined && {
        timeout_seconds: timeoutNumbers.timeoutSeconds
      }),
      ...(timeoutNumbers.threshold !== undefined && {
        consecutive_timeout_threshold: timeoutNumbers.threshold
      }),
      retry_delays_seconds: retryDelays
    }, true)
  } finally {
    updating.value = false
  }
}

const confirmDeletePlan = (plan: ScheduledTestPlan) => {
  deletingPlan.value = plan
  deleteRequiresOwnershipRestore.value = plan.owns_inactive_account
  showDeleteConfirm.value = true
}

const cancelDelete = () => {
  showDeleteConfirm.value = false
  deletingPlan.value = null
  deleteRequiresOwnershipRestore.value = false
}

const handleDelete = async () => {
  if (!deletingPlan.value) return
  const plan = deletingPlan.value
  try {
    if (deleteRequiresOwnershipRestore.value) {
      await adminAPI.scheduledTests.delete(plan.id, { restore_owned_account: true })
    } else {
      await adminAPI.scheduledTests.delete(plan.id)
    }
    appStore.showSuccess(t('admin.scheduledTests.deleteSuccess'))
    plans.value = plans.value.filter((candidate) => candidate.id !== plan.id)
    if (expandedPlanId.value === plan.id) {
      expandedPlanId.value = null
      results.value = []
    }
    cancelDelete()
  } catch (error: any) {
    if (!deleteRequiresOwnershipRestore.value && isConflict(error)) {
      deleteRequiresOwnershipRestore.value = true
      return
    }
    appStore.showError(error?.message || 'Failed to delete plan')
    cancelDelete()
  }
}

const toggleExpand = async (planId: number) => {
  if (expandedPlanId.value === planId) {
    expandedPlanId.value = null
    results.value = []
    expandedResultIds.clear()
    return
  }

  expandedPlanId.value = planId
  expandedResultIds.clear()
  await loadResults(planId)
}

const toggleResultDetail = (resultId: number) => {
  if (expandedResultIds.has(resultId)) {
    expandedResultIds.delete(resultId)
  } else {
    expandedResultIds.add(resultId)
  }
}
</script>

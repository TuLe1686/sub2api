<template>
  <AppLayout>
    <div class="mx-auto max-w-7xl px-4 py-6 sm:px-6 lg:px-8">
      <!-- 页头 -->
      <div class="mb-6">
        <h1 class="text-2xl font-bold text-gray-900 dark:text-gray-100">
          {{ t('admin.accountsEnhanced.title') }}
        </h1>
        <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
          {{ t('admin.accountsEnhanced.description') }}
        </p>
      </div>

      <!-- 刷新按钮 -->
      <div class="mb-4 flex items-center justify-between">
        <div class="flex items-center gap-2">
          <button
            class="btn btn-secondary px-3 py-1.5 text-sm"
            :disabled="loading"
            @click="loadAccounts"
          >
            <Icon v-if="loading" name="refresh" size="sm" class="animate-spin" />
            <Icon v-else name="refresh" size="sm" />
            <span class="ml-1">{{ t('common.refresh') }}</span>
          </button>
          <span v-if="accounts.length > 0" class="text-sm text-gray-500 dark:text-gray-400">
            {{ t('admin.accountsEnhanced.activeCount', { count: accounts.length }) }}
          </span>
        </div>
      </div>

      <!-- 加载中 -->
      <div v-if="loading && accounts.length === 0" class="flex justify-center py-12">
        <Icon name="refresh" size="lg" class="animate-spin text-gray-400" />
      </div>

      <!-- 空状态 -->
      <div
        v-else-if="accounts.length === 0"
        class="rounded-lg border border-dashed border-gray-300 py-12 text-center dark:border-dark-600"
      >
        <p class="text-gray-500 dark:text-gray-400">
          {{ t('admin.accountsEnhanced.noAccounts') }}
        </p>
      </div>

      <!-- 账号卡片列表 -->
      <div v-else class="grid grid-cols-1 gap-4 lg:grid-cols-2">
        <div
          v-for="account in accounts"
          :key="account.id"
          class="rounded-lg border border-gray-200 bg-white p-5 shadow-sm dark:border-dark-700 dark:bg-dark-800"
        >
          <!-- 账号标题行 -->
          <div class="mb-4 flex items-center justify-between border-b border-gray-100 pb-3 dark:border-dark-700">
            <div class="flex items-center gap-2">
              <span class="text-lg font-semibold text-gray-900 dark:text-gray-100">
                {{ account.name }}
              </span>
              <span class="rounded-full bg-green-100 px-2 py-0.5 text-xs text-green-700 dark:bg-green-900 dark:text-green-300">
                {{ account.platform }}
              </span>
            </div>
            <span class="text-xs text-gray-400">ID: {{ account.id }}</span>
          </div>

          <!-- 缓存率设置区 -->
          <div class="mb-4">
            <h3 class="mb-2 text-sm font-semibold text-gray-700 dark:text-gray-300">
              {{ t('admin.accountsEnhanced.cacheRate') }}
            </h3>
            <div class="flex items-center gap-3">
              <select
                v-model="account.config.cache_rate.mode"
                class="input input-sm w-32"
                @change="markDirty(account.id)"
              >
                <option value="off">{{ t('admin.accountsEnhanced.modes.off') }}</option>
                <option value="random">{{ t('admin.accountsEnhanced.modes.randomReduce') }}</option>
                <option value="fixed">{{ t('admin.accountsEnhanced.modes.fixedValue') }}</option>
              </select>

              <!-- 随机减少区间 -->
              <template v-if="account.config.cache_rate.mode === 'random'">
                <span class="text-xs text-gray-500">{{ t('admin.accountsEnhanced.reduceRange') }}</span>
                <input
                  v-model.number="account.config.cache_rate.random_reduce_min"
                  type="number"
                  min="1"
                  max="30"
                  class="input input-sm w-16"
                  @input="markDirty(account.id)"
                />
                <span class="text-gray-400">~</span>
                <input
                  v-model.number="account.config.cache_rate.random_reduce_max"
                  type="number"
                  min="1"
                  max="30"
                  class="input input-sm w-16"
                  @input="markDirty(account.id)"
                />
                <span class="text-xs text-gray-500">%</span>
              </template>

              <!-- 固定值 -->
              <template v-if="account.config.cache_rate.mode === 'fixed'">
                <span class="text-xs text-gray-500">{{ t('admin.accountsEnhanced.fixedValueLabel') }}</span>
                <input
                  v-model.number="account.config.cache_rate.fixed_value"
                  type="number"
                  min="0"
                  max="95"
                  class="input input-sm w-20"
                  @input="markDirty(account.id)"
                />
                <span class="text-xs text-gray-500">%</span>
              </template>
            </div>
          </div>

          <!-- 首字时长设置区 -->
          <div class="mb-4">
            <h3 class="mb-2 text-sm font-semibold text-gray-700 dark:text-gray-300">
              {{ t('admin.accountsEnhanced.ttftControl') }}
            </h3>
            <div class="flex items-center gap-3">
              <select
                v-model="account.config.ttft.mode"
                class="input input-sm w-40"
                @change="markDirty(account.id)"
              >
                <option value="off">{{ t('admin.accountsEnhanced.modes.off') }}</option>
                <option value="random">{{ t('admin.accountsEnhanced.modes.ttftRandom') }}</option>
                <option value="proportional">{{ t('admin.accountsEnhanced.modes.ttftProportional') }}</option>
              </select>

              <span v-if="account.config.ttft.mode === 'random'" class="text-xs text-gray-500">
                {{ t('admin.accountsEnhanced.ttftRandomHint') }}
              </span>
              <span v-if="account.config.ttft.mode === 'proportional'" class="text-xs text-gray-500">
                {{ t('admin.accountsEnhanced.ttftProportionalHint') }}
              </span>
            </div>
          </div>

          <!-- 保存按钮 -->
          <div class="flex justify-end border-t border-gray-100 pt-3 dark:border-dark-700">
            <button
              class="btn btn-primary px-4 py-1.5 text-sm"
              :class="{ 'opacity-50': !dirty[account.id], 'cursor-not-allowed': !dirty[account.id] }"
              :disabled="!dirty[account.id] || saving[account.id]"
              @click="saveAccount(account.id)"
            >
              <Icon v-if="saving[account.id]" name="refresh" size="sm" class="animate-spin" />
              <span>{{ t('common.save') }}</span>
            </button>
          </div>
        </div>
      </div>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { ref, reactive, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import Icon from '@/components/icons/Icon.vue'
import {
  list,
  getEnhancedControl,
  updateEnhancedControl,
  type EnhancedControl,
  type CacheRateSetting,
  type TTFTSetting,
} from '@/api/admin/accounts'
import type { Account } from '@/types'

const { t } = useI18n()

const loading = ref(false)
const accounts = ref<(Account & { config: EnhancedControl })[]>([])
const dirty = reactive<Record<number, boolean>>({})
const saving = reactive<Record<number, boolean>>({})

function defaultConfig(): EnhancedControl {
  return {
    cache_rate: { mode: 'off' } as CacheRateSetting,
    ttft: { mode: 'off' } as TTFTSetting,
  }
}

function markDirty(id: number) {
  dirty[id] = true
}

async function loadAccounts() {
  loading.value = true
  try {
    const resp = await list()
    // 只显示启用的账号（schedulable=true）
    const active = resp.items.filter((a) => a.schedulable)

    // 为每个账号加载增强配置
    accounts.value = await Promise.all(
      active.map(async (acc) => {
        let config = defaultConfig()
        try {
          const cfg = await getEnhancedControl(acc.id)
          config = {
            cache_rate: cfg.cache_rate ?? { mode: 'off' },
            ttft: cfg.ttft ?? { mode: 'off' },
          }
        } catch {
          // keep default
        }
        dirty[acc.id] = false
        return { ...acc, config }
      })
    )
  } finally {
    loading.value = false
  }
}

async function saveAccount(id: number) {
  saving[id] = true
  try {
    const acc = accounts.value.find((a) => a.id === id)
    if (!acc) return
    await updateEnhancedControl(id, {
      cache_rate: acc.config.cache_rate,
      ttft: acc.config.ttft,
    })
    dirty[id] = false
  } finally {
    saving[id] = false
  }
}

onMounted(loadAccounts)
</script>

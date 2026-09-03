<template>
  <main class="p-8">
    <button class="btn btn-primary" type="button" @click="showPanel = true">
      Open scheduled tests
    </button>
    <ScheduledTestsPanel
      :show="showPanel"
      :account-id="42"
      :model-options="modelOptions"
      @close="showPanel = false"
    />
  </main>
</template>

<script setup lang="ts">
import { ref } from 'vue'
import ScheduledTestsPanel from '../../src/components/admin/account/ScheduledTestsPanel.vue'

const showPanel = ref(false)
const modelOptions = [{ value: 'gpt-5.4', label: 'GPT-5.4' }]
</script>

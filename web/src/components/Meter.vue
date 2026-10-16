<script setup lang="ts">
import { computed } from 'vue'
import { Progress } from '@/components/ui/progress'
const p = defineProps<{ label: string; value: number | null | undefined; battery?: boolean; suffix?: string }>()
const color = computed(() => p.value == null ? 'meter-neutral' : p.battery ? (p.value <= 25 ? 'meter-red' : p.value <= 40 ? 'meter-amber' : 'meter-green') : p.value >= 85 ? 'meter-red' : p.value >= 65 ? 'meter-amber' : 'meter-green')
</script>
<template><div class="space-y-2"><div class="flex justify-between text-xs"><span class="muted">{{ label }}</span><span>{{ value == null ? '—' : Math.round(value)+'%' }}{{ suffix }}</span></div><Progress :model-value="value == null ? 0 : Math.min(100,Math.max(0,value))" :class="['h-1.5', color]" /></div></template>

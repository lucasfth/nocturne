<script setup lang="ts">
import { computed } from 'vue'
import type { Summary } from '@/api'
import { nf, sat, kr } from '@/format'
import { Card, CardContent } from '@/components/ui/card'
const props = defineProps<{ data: Summary }>()
const cards = computed(() => {
 const d = props.data, e = d.energy, w = d.wallet ?? { error: 'no wallet' }
 const tps = d.workers.reduce((a,w) => a + (w.eligible ? w.tokens_per_sec || 0 : 0), 0)
 return [
 ['Earned (30d)', sat(d.earned_msat), `≈ ${kr(d.earned_msat,d.dkk_per_btc)} · ${d.customers} paying · ${d.trial_users || 0} on trial · ${nf(d.days.reduce((a,x)=>a+x.requests,0))} requests`],
 ['Wallet', w.error ? (d.wallet ? 'offline' : 'none') : sat((w.balance_sat || 0)*1000), `≈ ${kr((w.balance_sat || 0)*1000,d.dkk_per_btc)} · ${w.channels ?? 0} channel${w.channels === 1 ? '' : 's'}`],
 ['Prepaid, unused', sat(d.customer_credit_liability_msat), 'owed to customers as service'],
 ['Power now', `${e.dkk_per_kwh === null ? '?' : nf(e.dkk_per_kwh,2)+' kr'}/kWh`, `${e.tier} tier · fleet ≈ ${e.dkk_per_kwh === null ? '?' : nf(e.serving_workers*d.watts_per_worker/1000*e.dkk_per_kwh,3)} kr/h`],
 ['Max revenue / h', kr(tps*3600*d.pricing.output_msat_per_1k_tokens/1000,d.dkk_per_btc), `if all Macs busy · ${nf(tps)} tok/s total`]]
})
const tests = computed(() => props.data.days.reduce((a,d)=>a+d.test_requests,0))
</script>
<template>
 <div class="grid gap-4 sm:grid-cols-2 xl:grid-cols-5">
  <Card v-for="(card,i) in cards" :key="card[0]" :class="i === 0 ? 'border-violet-400/60 bg-violet-500/5' : ''"><CardContent><p class="eyebrow">{{ card[0] }}</p><p class="my-3 text-2xl font-semibold tracking-tight">{{ card[1] }}</p><p class="muted text-xs leading-relaxed">{{ card[2] }}</p></CardContent></Card>
 </div>
 <p v-if="tests" class="muted mt-4 text-xs leading-relaxed">Internal tests (demo key, fake invoices) are excluded from earnings: {{ nf(tests) }} requests, {{ sat(data.test_usage_msat) }} of test credit. No real money has moved unless the Wallet shows it.</p>
</template>

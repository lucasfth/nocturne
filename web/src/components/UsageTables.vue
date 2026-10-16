<script setup lang="ts">
import type { Summary } from '@/api'
import { nf, sat, kr, ago } from '@/format'
import { Badge } from '@/components/ui/badge'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
defineProps<{ data: Summary; kind: 'customers' | 'daily' }>()
</script>
<template>
 <div class="rounded-xl border bg-card">
 <Table v-if="kind === 'customers'">
  <TableHeader><TableRow><TableHead v-for="h in ['Key','Requests','Spent','Paid in','Balance','Last used']" :key="h">{{ h }}</TableHead></TableRow></TableHeader>
  <TableBody><TableRow v-for="k in data.keys" :key="k.key_id"><TableCell class="font-mono">{{ k.key_id }} <Badge v-if="k.internal" variant="secondary">internal</Badge></TableCell><TableCell>{{ nf(k.requests) }}</TableCell><TableCell>{{ sat(k.spent_msat) }}</TableCell><TableCell>{{ sat(k.paid_in_msat) }}</TableCell><TableCell>{{ sat(k.balance_msat) }}</TableCell><TableCell>{{ ago(k.last_used) }}</TableCell></TableRow><TableRow v-if="!data.keys.length"><TableCell :colspan="6" class="p-8 text-center muted">No customers yet</TableCell></TableRow></TableBody>
 </Table>
 <Table v-else>
  <TableHeader><TableRow><TableHead v-for="h in ['Day','Paid requests','Test requests','Tokens in / out','Earned']" :key="h">{{ h }}</TableHead></TableRow></TableHeader>
  <TableBody><TableRow v-for="d in data.days" :key="d.day"><TableCell>{{ d.day }}</TableCell><TableCell>{{ nf(d.requests) }}</TableCell><TableCell>{{ nf(d.test_requests) }}</TableCell><TableCell>{{ nf(d.prompt_tokens) }} / {{ nf(d.completion_tokens) }}</TableCell><TableCell>{{ sat(d.revenue_msat) }} <span class="muted ml-2">{{ kr(d.revenue_msat,data.dkk_per_btc) }}</span></TableCell></TableRow><TableRow v-if="!data.days.length"><TableCell :colspan="5" class="p-8 text-center muted">No traffic yet</TableCell></TableRow></TableBody>
 </Table>
 </div>
</template>

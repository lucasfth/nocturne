<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { api, ApiError, type ConfigView, type ConfigValues, type SettingKey } from '@/api'
import { Button } from '@/components/ui/button'
const emit = defineEmits<{ saved: [] }>()
const keys: SettingKey[] = ['service_name', 'public_limit_concurrency']
const labels = { service_name: 'Service name', public_limit_concurrency: 'Public concurrency limit' }
const view = ref<ConfigView | null>(null)
const draft = ref({ service_name: '', public_limit_concurrency: '' })
const loading = ref(false), saving = ref(false), conflict = ref(false), unavailable = ref(false), error = ref(''), notice = ref('')
const changes = computed(() => {
 const result: Partial<ConfigValues> = {}
 if (!view.value) return result
 if (draft.value.service_name !== view.value.values.service_name) result.service_name = draft.value.service_name
 if (String(draft.value.public_limit_concurrency) !== String(view.value.values.public_limit_concurrency)) result.public_limit_concurrency = Number(draft.value.public_limit_concurrency)
 return result
})
const dirty = computed(() => Object.keys(changes.value).length > 0)
function supported(v: ConfigView) {
 return v.schema_version === 1 && typeof v.revision === 'string' && v.revision.length > 0 &&
  keys.every(k => v.fields?.[k] && typeof v.values?.[k] === (k === 'service_name' ? 'string' : 'number') && typeof v.effective?.[k] === typeof v.values[k]) &&
  v.fields.service_name.type === 'string' && v.fields.service_name.apply === 'live' &&
  v.fields.public_limit_concurrency.type === 'integer' && v.fields.public_limit_concurrency.apply === 'restart' &&
  Number.isInteger(v.fields.public_limit_concurrency.minimum) && Number.isInteger(v.fields.public_limit_concurrency.maximum) && Array.isArray(v.restart_required)
}
function accept(v: ConfigView) {
 if (!supported(v)) throw new Error('unsupported')
 view.value = v
 unavailable.value = false
 draft.value = { service_name: v.values.service_name, public_limit_concurrency: String(v.values.public_limit_concurrency) }
}
function failure(e: unknown) {
 // Do not render arbitrary server error bodies: they may include private configuration.
 if (e instanceof ApiError) {
  if (e.status === 401) return 'Authentication expired. Sign in again.'
  if (e.status === 403) return 'Access denied. Check admin authorization and same-origin proxy configuration.'
  if ([404,405,501].includes(e.status)) return 'Settings are not supported by this server (or mock mode).'
  if ([400,422].includes(e.status)) return 'The server rejected these values. Check the field constraints.'
  if (e.status >= 500) return 'Settings server unavailable. Reload before retrying; a failed save may require an operator restart.'
 }
 return 'Settings unavailable or unsupported response. Check the server and retry.'
}
async function load(preserve = false) {
 if (loading.value || saving.value) return
 const intent = { ...changes.value }
 loading.value = true; error.value = ''; notice.value = ''
 try {
  accept(await api.config())
  if (preserve) {
   for (const k of keys) if (k in intent) draft.value[k] = String(intent[k])
   notice.value = 'Latest saved values loaded. Your changed fields are preserved. Review them against Saved and Effective before saving again.'
  }
  conflict.value = false
 } catch (e) { unavailable.value = true; error.value = failure(e) }
 finally { loading.value = false }
}
function validation() {
 const name = draft.value.service_name, field = view.value!.fields.public_limit_concurrency
 if ([...name].length < 1 || [...name].length > 80 || /[<>&"'\p{C}\p{Zl}\p{Zp}]/u.test(name) || /[^\S ]/u.test(name)) return 'Service name must contain 1–80 printable characters, without < > & quotes or non-space whitespace.'
 const value = Number(draft.value.public_limit_concurrency)
 if (!/^\d+$/.test(draft.value.public_limit_concurrency) || !Number.isInteger(value) || value < field.minimum! || value > field.maximum!) return `Public concurrency limit must be an integer from ${field.minimum} to ${field.maximum}.`
 return ''
}
async function save() {
 if (!view.value || saving.value || loading.value || conflict.value || unavailable.value || !dirty.value) return
 error.value = validation(); notice.value = ''
 if (error.value) return
 saving.value = true
 try { accept(await api.saveConfig({ revision: view.value.revision, changes: changes.value })); notice.value = 'Settings saved. Live settings are effective now; pending settings need an operator restart.'; emit('saved') }
 catch (e) { if (e instanceof ApiError && e.status === 409) { conflict.value = true; error.value = 'Configuration changed elsewhere. No changes were applied by this request. Reload latest values, preserve your edits, then review and save explicitly. External file edits may require an operator restart.' } else { error.value = failure(e); if (!(e instanceof ApiError) || e.status >= 500 || [403,404,405,501].includes(e.status)) unavailable.value = true } }
 finally { saving.value = false }
}
onMounted(() => load())
</script>
<template>
 <section aria-labelledby="settings-title" class="max-w-3xl space-y-5 rounded-xl border p-5">
  <h2 id="settings-title" class="text-xl font-semibold">Settings</h2>
  <p class="text-sm muted">Two safe settings are currently editable. Credentials, payment backends, models, paths, access control and other configuration are not editable here. Secrets are never fetched or displayed.</p>
  <p v-if="loading" role="status">Loading settings…</p>
  <p v-if="error" role="alert" class="text-sm text-destructive">{{ error }}</p>
  <p v-if="notice" role="status" class="text-sm">{{ notice }}</p>
  <Button type="button" variant="outline" :disabled="loading || saving" @click="load(true)">Reload latest (keep edits)</Button>
  <form v-if="view" class="space-y-6" novalidate @submit.prevent="save">
   <fieldset :disabled="saving || loading || unavailable" class="space-y-6">
    <div v-for="key in keys" :key="key" class="space-y-2">
     <label :for="`setting-${key}`" class="block font-medium">{{ labels[key] }}</label>
     <input :id="`setting-${key}`" v-model="draft[key]" :type="view.fields[key].type === 'integer' ? 'number' : 'text'" :min="view.fields[key].minimum ?? undefined" :max="view.fields[key].maximum ?? undefined" :step="key === 'public_limit_concurrency' ? 1 : undefined" :aria-describedby="`help-${key}`" required class="w-full rounded-md border bg-transparent px-3 py-2" />
     <div :id="`help-${key}`" class="text-sm muted">
      <p>{{ view.fields[key].type }} · {{ view.fields[key].apply === 'live' ? 'Applies live' : 'Requires operator restart' }}</p>
      <p v-if="key === 'service_name'">1–80 printable characters; no &lt; &gt; &amp; quotes or non-space whitespace.</p>
      <p v-else>Minimum {{ view.fields[key].minimum }} · Maximum {{ view.fields[key].maximum }} · Whole numbers only</p>
      <p>Default: {{ view.fields[key].default }}</p>
      <p :data-testid="`saved-${key}`">Saved: {{ view.values[key] }}</p>
      <p :data-testid="`effective-${key}`">Effective: {{ view.effective[key] }}</p>
     </div>
     <p v-if="view.restart_required.includes(key)" class="text-sm font-medium">Restart pending: {{ labels[key] }} is saved but not effective.</p>
    </div>
   </fieldset>
   <p class="text-sm muted">This page cannot restart the gateway. An operator must restart it through the installation's service manager. Reload afterwards to verify effective values. Manual file changes also require a restart; do not share a configuration file between running coordinators.</p>
   <Button type="submit" :disabled="!dirty || saving || loading || conflict || unavailable">{{ saving ? 'Saving…' : 'Save settings' }}</Button>
  </form>
 </section>
</template>

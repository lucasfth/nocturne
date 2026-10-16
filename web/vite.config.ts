import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import tailwindcss from '@tailwindcss/vite'
import { fileURLToPath, URL } from 'node:url'
export default defineConfig({ base: '/admin/', define: { __MOCK__: JSON.stringify(process.env.VITE_MOCK === '1') }, build: { sourcemap: false, assetsInlineLimit: 0 }, plugins: [vue(), tailwindcss()], resolve: { alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) } }, server: { proxy: { '^/admin/(summary|machines|ledger|config)(/|$)': 'http://127.0.0.1:8702' } } })

import { defineConfig } from '@playwright/test'
export default defineConfig({
 testDir: './tests', fullyParallel: true,
 use: { baseURL: 'http://127.0.0.1:5199', channel: 'chrome', screenshot: 'only-on-failure' },
 webServer: [{ command: 'sh tests/run-go-benchmark-server.sh', url: 'http://127.0.0.1:5201/admin', reuseExistingServer: false }, { command: 'bun run preview --host 127.0.0.1 --port 5199 --strictPort', url: 'http://127.0.0.1:5199/admin/', reuseExistingServer: false }, { command: `${process.env.NOCTURNE_TEST_PYTHON || '../.venv/bin/python'} tests/python-settings-server.py`, url: 'http://127.0.0.1:5200/admin', reuseExistingServer: false }],
})

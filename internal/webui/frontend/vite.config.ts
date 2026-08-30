import { defineConfig } from 'vite'
import preact from '@preact/preset-vite'

export default defineConfig({
  plugins: [preact()],
  build: {
    outDir: '../static',
    emptyOutDir: true,
    assetsDir: 'assets',
    sourcemap: false
  },
  server: {
    proxy: {
      '/api': 'http://127.0.0.1:8080'
    }
  },
  test: {
    environment: 'node',
    include: ['test/**/*.test.ts']
  }
})

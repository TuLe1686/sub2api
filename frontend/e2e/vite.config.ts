import { defineConfig, type Plugin } from 'vite'
import vue from '@vitejs/plugin-vue'
import { resolve } from 'path'

const stripVueStyles = (): Plugin => ({
  name: 'e2e-strip-vue-styles',
  enforce: 'pre',
  transform(source, id) {
    if (!id.endsWith('.vue')) return
    return source.replace(/<style\b[^>]*>[\s\S]*?<\/style>/g, '')
  }
})

export default defineConfig({
  root: resolve(__dirname, '..'),
  publicDir: false,
  plugins: [stripVueStyles(), vue()],
  resolve: {
    alias: {
      '@': resolve(__dirname, '../src'),
      'vue-i18n': 'vue-i18n/dist/vue-i18n.runtime.esm-bundler.js'
    }
  },
  define: {
    __INTLIFY_JIT_COMPILATION__: true
  },
  server: {
    host: '127.0.0.1',
    port: 4173,
    strictPort: true
  }
})

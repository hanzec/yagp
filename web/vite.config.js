import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import Components from 'unplugin-vue-components/vite'
import { ElementPlusResolver } from 'unplugin-vue-components/resolvers'
import path from 'path'

// https://vite.dev/config/
export default defineConfig({
  plugins: [vue(), Components({ resolvers: [ElementPlusResolver()], dts: false })],
  resolve: {
    alias: {
      '@': path.resolve(__dirname, 'src')
    }
  },
  server: {
    port: 5173,
    fs: { allow: [__dirname, path.resolve(__dirname, '../pkg/gost/relay_options.json')] },
    proxy: {
      '/api': {
        target: 'http://localhost:39100'
      }
    }
  },
  build: {
    outDir: '../internal/router/dist',
    emptyOutDir: true
  }
})

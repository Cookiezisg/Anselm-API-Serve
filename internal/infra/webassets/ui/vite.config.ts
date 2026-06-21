import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// base '/static/': the Go dashboard serves embedded assets under GET /static/*
// (handleStatic maps /static/<file> → embed root). Emitting asset URLs as
// /static/assets/... lets the existing route serve the built JS/CSS unchanged,
// while index.html itself is served by serveSPA on every non-API GET.
//
// base '/static/':后端 handleStatic 把 /static/<file> 映射到 embed 根,故让 Vite
// 产物引用 /static/assets/...,复用既有静态路由(只换 embed 目录,Go 路由不动)。
//
// dev proxy: `npm run dev` 把 /login·/logout·/api·/static 转给已跑的后端
// (ANSELM_DASHBOARD_URL,默认 127.0.0.1:8081),本地调试无需切 embed。
const backend = process.env.ANSELM_DASHBOARD_URL || 'http://127.0.0.1:8081'

export default defineConfig({
  plugins: [react()],
  base: '/static/',
  build: {
    outDir: 'dist',
    emptyOutDir: true,
  },
  server: {
    proxy: {
      '/login': backend,
      '/logout': backend,
      '/api': backend,
      '/healthz': backend,
    },
  },
})

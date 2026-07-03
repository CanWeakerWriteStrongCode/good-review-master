import { defineConfig } from 'vite'
import uni from '@dcloudio/vite-plugin-uni'

export default defineConfig({
  plugins: [
    uni(),
    // uni-app 的 H5 插件会覆盖 chunkFileNames，用 enforce: 'post' 反覆盖
    {
      name: 'go-embed-fix',
      enforce: 'post',
      config() {
        return {
          build: {
            rollupOptions: {
              output: {
                // Go embed 排除 _ / . 开头的文件
                chunkFileNames(chunkInfo) {
                  const name = chunkInfo.name.replace(/^_/, 'chunk-')
                  return `assets/${name}.[hash].js`
                },
              },
            },
          },
        }
      },
    },
  ],
  server: {
    port: 5173,
    proxy: {
      '/api': {
        target: 'http://localhost:8080',
        changeOrigin: true,
      },
    },
  },
  build: {
    outDir: '../server/static/frontend',
  },
})

import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

export default defineConfig({
  base: '/admin/cpa/assets/',
  plugins: [react()],
  build: {
    outDir: '../../internal/server/assets/cpa',
    emptyOutDir: true,
    target: 'es2020',
    cssCodeSplit: false,
    rolldownOptions: {
      output: {
        entryFileNames: 'panel.js',
        chunkFileNames: 'panel-[name].js',
        assetFileNames: 'panel.[ext]',
      },
    },
  },
});

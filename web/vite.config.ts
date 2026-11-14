import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// The built files in dist/ are embedded in buttercams.exe and served at "/".
// The server falls back to index.html for every path outside /api, so asset
// URLs must be absolute (base "/").
export default defineConfig({
  base: '/',
  plugins: [react()],
  build: {
    outDir: 'dist',
    emptyOutDir: true,
  },
  server: {
    proxy: {
      // Development only: forward API calls to a locally running guardian,
      // which uses a self-signed certificate by default.
      '/api': {
        target: 'https://localhost:8650',
        secure: false,
      },
    },
  },
});

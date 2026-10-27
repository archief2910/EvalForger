import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    // Vite rejects unknown Host headers. Allow the temporary tunnel domains
    // used by `cloudflared tunnel --url` and localhost.run so a local demo
    // can be shared without deploying.
    allowedHosts: [
      ".trycloudflare.com",
      ".lhr.life",
      ".localhost.run",
      ".ngrok-free.app",
      ".ngrok.io",
    ],
    proxy: {
      "/api": { target: "http://localhost:8000", changeOrigin: true },
    },
  },
});

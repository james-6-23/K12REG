# Frontend

Vue 3 + Vite + TypeScript + Tailwind。

```bash
npm install
npm run dev      # http://127.0.0.1:5173 → 代理 /api 到 :8000
npm run build    # → ./dist（Go 默认托管路径）
```

仓库根启动 API：

```bash
go run ./cmd/server serve -data ./data -static ./frontend/dist -password admin -addr :8000
```

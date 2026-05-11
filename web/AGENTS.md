# WEB FRONTEND

React 18 + TypeScript + Vite 7 + Tailwind CSS dashboard for Clawvisor.

## STRUCTURE

```
web/
├── src/
│   ├── main.tsx           # Entry point (BrowserRouter + QueryClient + AuthProvider)
│   ├── App.tsx            # Route definitions, ErrorBoundary, RequireAuth guard
│   ├── api/
│   │   └── client.ts      # Typed API client (all backend calls go through here)
│   ├── components/        # Shared UI components (badges, cards, icons, pills)
│   ├── hooks/
│   │   ├── useAuth.tsx    # Auth context provider (access token in memory, refresh in localStorage)
│   │   ├── useEventStream.ts  # SSE real-time updates via ticket auth
│   │   └── useTheme.ts    # Light/dark mode toggle
│   ├── lib/               # Pure helpers (env, queue logic, service metadata, webauthn)
│   ├── pages/             # Route-level views (Dashboard, Tasks, Services, etc.)
│   └── index.css          # Tailwind directives + CSS custom properties for theming
├── index.html
├── vite.config.ts
├── tailwind.config.ts
├── tsconfig.json
└── postcss.config.js
```

## WHERE TO LOOK

| Task | Location |
|------|----------|
| Add a page/route | `src/pages/`, then wire in `src/App.tsx` |
| Add a shared component | `src/components/` |
| Add an API call | `src/api/client.ts` (add to the `api` object) |
| Add a custom hook | `src/hooks/` |
| Change theming/colors | `src/index.css` (CSS vars) + `tailwind.config.ts` (color tokens) |
| Change proxy targets | `vite.config.ts` |
| Change TypeScript strictness | `tsconfig.json` |

## COMMANDS

```bash
make web-dev          # Vite dev server on :8080 (proxies /api to localhost:25297)
npm run dev           # Vite dev server on :5173 (same proxy config)
npm run build         # tsc -b && vite build (output to dist/)
npm run lint          # tsc --noEmit (type-only check, no eslint)
```

## CONVENTIONS

- All backend requests go through `src/api/client.ts`. Never call `fetch` directly in pages or components.
- Access tokens live in React state (memory). Refresh tokens in `localStorage`. On 401, the client silently refreshes and retries once.
- SSE real-time updates use ticket auth (`/api/events/ticket`), not the JWT in the URL.
- Dark mode uses Tailwind `darkMode: 'class'`. Toggle is in `useTheme.ts`.
- Custom Tailwind colors reference CSS custom properties with `<alpha-value>` support (e.g. `surface-0`, `brand`, `danger`). Define new colors in both `index.css` and `tailwind.config.ts`.
- React Query stale time is 30s, retry is 1. Use `useQuery`/`useMutation` for data fetching.
- Route guards: `RequireAuth` wrapper in `App.tsx` redirects unauthenticated users based on `authMode`.
- Feature flags from `/api/features` control which routes render (e.g. `password_auth` gates `/login`).

## ANTI-PATTERNS

- Never bypass `api/client.ts` with raw `fetch` calls.
- Never store access tokens in `localStorage` (refresh tokens only).
- Never hardcode color values in JSX. Use the Tailwind custom color tokens.
- Never add new CSS custom properties without the corresponding `tailwind.config.ts` entry.
- Never import page components outside `App.tsx` route definitions.

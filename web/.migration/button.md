# button

2026-10-19 · 采用官方 base-nova registry 原文（registry 拉取 + 手工改写 cn 导入）；基座已切到 Base UI，消费方无需改调用点。

## Changed

- `web/components.json:3` — style `new-york`（legacy radix）→ `base-nova`。schema 没有独立 `base` 字段，基座由 style 前缀决定。
- `web/package.json` / `web/pnpm-lock.yaml` — 新增 `@base-ui/react@1.9.0`。迁移前项目没有任何 `@radix-ui/*` 依赖，因此无 radix 包可移除。
- `web/src/components/ui/button.tsx:1` — 改为真正的 `@base-ui/react/button` 原语（`ButtonPrimitive`），不是手写 useRender 包装。保留官方 base-nova 全部变体（default/outline/secondary/ghost/destructive/link）与尺寸（default/xs/sm/lg/icon/icon-*），`buttonVariants` 继续导出。
- `web/src/components/ui/button.tsx:3` — registry 原文 `import { cn } from "cn"` 改写为项目既有的 `@/lib/utils`；未引入 `cn` npm 包。文件其余内容与 `https://ui.shadcn.com/r/styles/base-nova/button.json` 逐字一致。
- 没有使用 `shadcn add --overwrite`：该 CLI 的 dry-run 会额外安装 `cn` 包并保留 `from "cn"` 导入，和项目既有 utils 冲突。
- 残留扫描：`grep -n "radix-ui\|@radix-ui" web/src/components/ui/button.tsx` 无匹配。

## Left alone

- 17 个消费文件未改：调用点只用到 `variant`（default/outline/ghost）与 `size`（default/sm），base-nova 均支持；`asChild` 从未使用。
- `buttonVariants` 在 5 处用于 `<Link>` / `<a>` 的 class（如 `_authed.sandboxes.index.tsx:61`），它是纯 class 生成函数，继续可用。
- `web/src/index.css` 未动，保留 new-york 主题令牌。base-nova 的 `secondary` 变体引用 `--secondary`（当前未定义），但项目未使用该变体。

## Behavior changes

- 尺寸/外观按官方模板变化：默认高度 h-9 → h-8，sm h-8 → h-7，圆角 md → lg，hover primary 90% → 80%，focus ring 2px → 3px。
- Base UI 原语新增 `data-slot="button"` 和按下位移（`active:not-aria-[haspopup]:translate-y-px`）；disabled 仍不响应指针。
- wrapper props 类型变为 `ButtonPrimitive.Props`，因此 `render` 属性可用（原 wrapper 本来也不支持 `asChild`）。
- 无菜单/对话框相关行为变化（项目没有这些组件）。

## Verify by hand

- 点普通按钮：有轻微按下位移，hover 颜色变化可见。
- Tab 到按钮：3px focus ring；Enter/Space 都能触发。
- 审核/节点页的 `variant="outline" size="sm"` 按钮高度、间距正常。
- `buttonVariants()` 渲染的入口 `<Link>` 外观与 Button 一致。
- 禁用按钮不可点击、无 pointer 光标。

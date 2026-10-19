# project

2026-10-19 · 整项目基座切换：`new-york`（legacy radix）→ `base-nova`（Base UI）。迁移前项目没有任何 radix 依赖或源码引用，因此本次是“采用 Base UI 模板”而非“拆 radix”。

## Dependency swap

- 新增 `@base-ui/react@1.9.0`（pnpm）。
- 移除 radix 依赖：无。全仓库 `grep -rn "@radix-ui\|radix-ui"` 在迁移前就是 0 命中。
- `web/components.json` style `new-york` → `base-nova`；schema 没有独立 `base` 字段，基座由 style 前缀决定。
- 未安装 registry 建议的 `cn` npm 包；三个 wrapper 的 `cn` 导入统一指向既有 `@/lib/utils`。

## Wrapper migration

- `button`、`input`、`label` 三个 wrapper 均以官方 base-nova registry 原文覆盖；详见同目录各自报告。
- 没有生成 `-base.tsx` 渐进文件：项目本来就没有 radix 包装，无需 strangler 过渡。

## App-code sweep

- 调用点只用到 `variant`（default/outline/ghost）、`size`（default/sm）和原生 input/label props；无 `asChild`、无 radix 专属属性，因此应用代码零改动。
- `buttonVariants` 继续导出，5 个文件用它给 `<Link>` 生成按钮样式。

## Flags / follow-ups

- `web/src/index.css` 仍是 new-york 主题令牌。base-nova 的 `secondary`/`accent` 等变体依赖 `--secondary`、`--accent` 等变量（当前未定义），项目当前未使用这些变体；若以后要用，需要按 base-nova 主题补齐 CSS。
- `web/src/lib/utils.ts` 的 `selectClass` 仍按旧 h-9 输入高度手写，与新 Input（h-8）不一致；本次未动。
- 非 radix 库（cmdk/vaul/sonner/input-otp/react-day-picker/recharts）本项目均未使用，无需处理。

## Final verification

- `pnpm typecheck`：通过。
- `pnpm build`：通过。
- `pnpm test`：12 个测试文件、83 个测试全部通过。
- 剩余 Radix wrapper：**0**。

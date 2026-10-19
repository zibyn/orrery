# input

2026-10-19 · 采用官方 base-nova registry 原文；基座已切到 Base UI，消费方无需改调用点。

## Changed

- `web/src/components/ui/input.tsx:2` — 改为 `@base-ui/react/input` 原语（`InputPrimitive`），保留官方 base-nova 类名。
- `web/src/components/ui/input.tsx:3` — `from "cn"` 改写为 `@/lib/utils`；删除 `"use client"` 指令（Vite SPA 不需要，项目其他文件也没有）。
- `web/components.json` / `package.json` / lockfile 的基座切换记录见 `button.md`（同一次迁移）。
- 残留扫描：`grep -n "radix-ui\|@radix-ui" web/src/components/ui/input.tsx` 无匹配。

## Left alone

- 全部消费点（login、sandboxes.new、admin.audit、admin.users、terminal）只用原生 props：`id`、`type`、`value`、`onChange`、`placeholder`、`disabled`，无 ref；无需改动。
- `web/src/lib/utils.ts` 的 `selectClass` 仍是旧 h-9 样式，未同步——它不是 shadcn wrapper，避免把改动扩散到非迁移文件。

## Behavior changes

- 高度 h-9 → h-8；圆角 md → lg；字号 text-base，md 断点以上 text-sm；focus 由 ring-2 改为 border + ring-3；禁用态增加 `bg-input/50`。
- Base UI Input 额外支持 `onValueChange` 与 Field 集成；原有 `onChange` 照常可用，受控/非受控行为不变。

## Verify by hand

- 登录页两个输入框可正常输入，密码遮罩正常，提交校验不变。
- 新建 sandbox 的 number 输入框可编辑，提交校验不变。
- 审计过滤页：actor/action/target 文本与 datetime-local 都可输入。
- 禁用输入框呈灰底且不可编辑；聚焦时边框和 ring 清晰可见。

# label

2026-10-19 · 采用官方 base-nova registry 原文；保持原生 `<label>`（Base UI 无 Label 原语，符合硬规则）。

## Changed

- `web/src/components/ui/label.tsx:4` — 替换为官方 base-nova 原生 label：增加 `data-slot="label"`、`flex items-center gap-2`、`select-none`、`peer-disabled:*` 钩子。
- `from "cn"` 改写为 `@/lib/utils`；删除 `"use client"`。
- 残留扫描：`grep -n "radix-ui\|@radix-ui" web/src/components/ui/label.tsx` 无匹配。

## Left alone

- 消费方的 `htmlFor` 用法不变（原生 label 语义）。
- 未引入任何 Base UI 组件：Label 在 Base UI 没有对应原语。

## Behavior changes

- label 现在默认 `flex` + `gap-2`；若以后在 label 内放图标或多段文本会变成横排。当前调用点都是纯文本，无影响。
- `select-none` 使 label 文本不可被拖选。

## Verify by hand

- 点击 “Command”“Actor”“Current password” 等 label，焦点落到对应输入框。
- 表单布局没有因 flex 产生错位，文本不可拖选。

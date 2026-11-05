// Unix 时间戳与日期互转。
//
// 两个容易出错的点：
//   1. JS 的 Date 用毫秒，Unix 时间戳惯例是秒。差 1000 倍是最常见的 bug。
//   2. new Date("2026-10-30") 按 UTC 解析字符串日期，但
//      new Date("2026-10-30T00:00") 按本地时区。<input type="datetime-local">
//      给的是后者，所以这里统一用 getTimezoneOffset 补偿。
//
// 显示上刻意同时给出秒与毫秒、本地与 UTC——排查跨时区问题时
// 少一次换算就少一次错。

import { h } from "../lib/dom";
import type { ToolFactory } from "./types";
import {
  actions,
  button,
  copyButton,
  errorLine,
  field,
  select,
  setStats,
  showError,
  stats,
  textInput,
} from "./shared";

/** 把 Date 补成 <input type="datetime-local"> 需要的 "YYYY-MM-DDTHH:mm[:ss]"。 */
function toLocalInputValue(date: Date): string {
  const pad = (n: number): string => String(n).padStart(2, "0");
  return (
    `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}` +
    `T${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`
  );
}

/** ISO 8601 带时区偏移，便于粘贴到别处。 */
function toOffsetISO(date: Date): string {
  const pad = (n: number): string => String(n).padStart(2, "0");
  const offset = -date.getTimezoneOffset();
  const sign = offset >= 0 ? "+" : "-";
  const abs = Math.abs(offset);
  return (
    `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}` +
    `T${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}` +
    `${sign}${pad(Math.floor(abs / 60))}:${pad(abs % 60)}`
  );
}

export const factory: ToolFactory = (ctx) => {
  const tsInput = textInput("例如 1793361448", { mono: true });
  const unit = select([
    ["auto", "自动识别（10 位秒 / 13 位毫秒）"],
    ["s", "秒"],
    ["ms", "毫秒"],
  ]);
  const dateInput = textInput("", { type: "datetime-local" });
  const error = errorLine();
  const statLine = stats();

  /** 时间戳 → 日期输入框与统计。 */
  const fromTimestamp = (): void => {
    const raw = tsInput.value.trim();
    if (raw === "") {
      showError(error, "");
      setStats(statLine, []);
      return;
    }
    if (!/^-?\d+$/u.test(raw)) {
      showError(error, "时间戳必须是整数（可带负号表示 1970 年之前）");
      setStats(statLine, []);
      return;
    }

    const n = Number(raw);
    if (!Number.isSafeInteger(n)) {
      showError(error, "数值超出安全整数范围，精度会丢失");
      setStats(statLine, []);
      return;
    }

    let ms: number;
    if (unit.value === "s") ms = n * 1000;
    else if (unit.value === "ms") ms = n;
    else ms = Math.abs(n) >= 1e12 ? n : n * 1000; // 1e12 ms ≈ 2001 年

    const date = new Date(ms);
    if (Number.isNaN(date.getTime())) {
      showError(error, "时间戳超出可表示范围");
      setStats(statLine, []);
      return;
    }

    showError(error, "");
    dateInput.value = toLocalInputValue(date);

    setStats(statLine, [
      ["本地", date.toLocaleString("zh-CN")],
      ["UTC", date.toISOString()],
      ["相对现在", relative(date)],
    ]);
  };

  /** 日期输入框 → 时间戳。 */
  const fromDate = (): void => {
    if (dateInput.value === "") return;
    // datetime-local 的值没有时区，new Date 会按本地时区解释，这与预期一致。
    const date = new Date(dateInput.value);
    if (Number.isNaN(date.getTime())) {
      showError(error, "无法解析该日期");
      return;
    }
    showError(error, "");
    tsInput.value = String(Math.floor(date.getTime() / 1000));
    // 写入的是秒：单位若停在"毫秒"，之后改动单位或时间戳时会按毫秒解读，差 1000 倍。
    unit.value = "s";
    setStats(statLine, [
      ["秒", String(Math.floor(date.getTime() / 1000))],
      ["毫秒", String(date.getTime())],
      ["本地", date.toLocaleString("zh-CN")],
      ["UTC", date.toISOString()],
      ["偏移", toOffsetISO(date)],
    ]);
  };

  tsInput.addEventListener("input", fromTimestamp);
  unit.addEventListener("change", fromTimestamp);
  dateInput.addEventListener("input", fromDate);

  const element = h(
    "div",
    { class: "tool-body" },
    h(
      "div",
      { class: "tool-grid-2" },
      field("时间戳", tsInput, "10 位是秒，13 位是毫秒"),
      field("日期时间（本地时区）", dateInput),
    ),
    actions(
      unit,
      button(
        "当前时间",
        () => {
          const now = new Date();
          tsInput.value = String(Math.floor(now.getTime() / 1000));
          unit.value = "s";
          fromTimestamp();
        },
        "primary",
      ),
      button("清空", () => {
        tsInput.value = "";
        dateInput.value = "";
        showError(error, "");
        setStats(statLine, []);
      }),
      copyButton(() => tsInput.value, ctx.toast, "复制时间戳"),
      copyButton(() => dateInput.value, ctx.toast, "复制日期"),
    ),
    error,
    statLine,
  );

  fromTimestamp();
  return { element };
};

/** 相对现在的模糊描述，只在合理范围内给出。 */
function relative(date: Date): string {
  const diff = date.getTime() - Date.now();
  const abs = Math.abs(diff);
  if (abs < 60_000) return "刚刚";
  const units: ReadonlyArray<readonly [number, string]> = [
    [86_400_000, "天"],
    [3_600_000, "小时"],
    [60_000, "分钟"],
  ];
  for (const [size, label] of units) {
    if (abs >= size) {
      const n = Math.round(abs / size);
      return diff > 0 ? `${n} ${label}后` : `${n} ${label}前`;
    }
  }
  return "刚刚";
}

export default factory;

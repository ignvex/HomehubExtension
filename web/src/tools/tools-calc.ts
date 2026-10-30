// 程序员计算器：HEX / DEC / OCT / BIN 同步显示。
//
// 为什么用 BigInt 而不是 Number：JS 的 Number 只有 53 位精度，
// 而 64 位整数（0xFFFFFFFFFFFFFFFF）远超这个范围。用 Number
// 算 2^63 会静默丢精度，算位运算会得到错误结果。
//
// 支持：+ - * / % **、位运算 & | ^ << >>、一元 ~ 与负号、括号。

import { h } from "../lib/dom";
import type { ToolFactory } from "./types";
import { actions, button, copyButton, errorLine, field, setStats, showError, stats, textInput } from "./shared";

const MASK64 = (1n << 64n) - 1n;
const SIGN64 = 1n << 63n;

/** 把 BigInt 按位宽截断到 64 位。位运算必须这么做。 */
function wrap64(v: bigint): bigint {
  const masked = v & MASK64;
  return masked >= SIGN64 ? masked - (1n << 64n) : masked;
}

type Token =
  | { kind: "num"; value: bigint }
  | { kind: "op"; value: string }
  | { kind: "lparen" }
  | { kind: "rparen" };

/** 数字字面量：支持 0x / 0o / 0b 前缀与十进制。 */
function tokenize(input: string): Token[] {
  const tokens: Token[] = [];
  let i = 0;
  while (i < input.length) {
    const ch = input[i] ?? "";
    if (/\s/u.test(ch)) {
      i += 1;
      continue;
    }
    if (ch === "(") {
      tokens.push({ kind: "lparen" });
      i += 1;
      continue;
    }
    if (ch === ")") {
      tokens.push({ kind: "rparen" });
      i += 1;
      continue;
    }
    // 多字符运算符优先匹配，否则 "<<" 会被读成两个 "<"。
    const two = input.slice(i, i + 2);
    if (two === "<<" || two === ">>" || two === "**") {
      tokens.push({ kind: "op", value: two });
      i += 2;
      continue;
    }
    if ("+-*/%&|^~".includes(ch)) {
      tokens.push({ kind: "op", value: ch });
      i += 1;
      continue;
    }
    const numMatch = /^(0[xX][0-9a-fA-F]+|0[oO][0-7]+|0[bB][01]+|\d+)/u.exec(input.slice(i));
    if (numMatch) {
      const raw = numMatch[0];
      let value: bigint;
      if (/^0[xX]/u.test(raw)) value = BigInt(raw);
      else if (/^0[oO]/u.test(raw)) value = BigInt(raw);
      else if (/^0[bB]/u.test(raw)) value = BigInt(raw);
      else value = BigInt(raw);
      tokens.push({ kind: "num", value });
      i += raw.length;
      continue;
    }
    throw new Error(`无法识别的字符「${ch}」`);
  }
  return tokens;
}

/** 递归下降求值。优先级从低到高：| ^ & 移位 加减 乘除 一元。 */
function evaluate(input: string): bigint {
  const tokens = tokenize(input);
  let pos = 0;

  const peek = (): Token | undefined => tokens[pos];
  const eat = (): Token => {
    const t = tokens[pos];
    if (!t) throw new Error("表达式不完整");
    pos += 1;
    return t;
  };

  const parseOr = (): bigint => {
    let left = parseXor();
    for (;;) {
      const t = peek();
      if (t?.kind === "op" && t.value === "|") {
        eat();
        left = wrap64(left | parseXor());
      } else break;
    }
    return left;
  };

  const parseXor = (): bigint => {
    let left = parseAnd();
    for (;;) {
      const t = peek();
      if (t?.kind === "op" && t.value === "^") {
        eat();
        left = wrap64(left ^ parseAnd());
      } else break;
    }
    return left;
  };

  const parseAnd = (): bigint => {
    let left = parseShift();
    for (;;) {
      const t = peek();
      if (t?.kind === "op" && t.value === "&") {
        eat();
        left = wrap64(left & parseShift());
      } else break;
    }
    return left;
  };

  const parseShift = (): bigint => {
    let left = parseAdd();
    for (;;) {
      const t = peek();
      if (t?.kind === "op" && (t.value === "<<" || t.value === ">>")) {
        const op = eat();
        const right = parseAdd();
        const shift = Number(BigInt.asUintN(64, right) & 63n);
        // 移位量按 64 位取模，与大多数语言的行为一致。
        left = op.kind === "op" && op.value === "<<" ? wrap64(left << BigInt(shift)) : wrap64(left >> BigInt(shift));
      } else break;
    }
    return left;
  };

  const parseAdd = (): bigint => {
    let left = parseMul();
    for (;;) {
      const t = peek();
      if (t?.kind === "op" && (t.value === "+" || t.value === "-")) {
        const op = eat();
        const right = parseMul();
        left = wrap64(op.kind === "op" && op.value === "+" ? left + right : left - right);
      } else break;
    }
    return left;
  };

  const parseMul = (): bigint => {
    let left = parseUnary();
    for (;;) {
      const t = peek();
      if (t?.kind === "op" && (t.value === "*" || t.value === "/" || t.value === "%" || t.value === "**")) {
        const op = eat();
        const right = parseUnary();
        if (op.kind === "op") {
          if (op.value === "*") left = wrap64(left * right);
          else if (op.value === "**") left = wrap64(left ** right);
          else if (op.value === "/") {
            if (right === 0n) throw new Error("除以零");
            left = wrap64(left / right);
          } else {
            if (right === 0n) throw new Error("对零取模");
            left = wrap64(left % right);
          }
        }
      } else break;
    }
    return left;
  };

  const parseUnary = (): bigint => {
    const t = peek();
    if (t?.kind === "op" && t.value === "-") {
      eat();
      return wrap64(-parseUnary());
    }
    if (t?.kind === "op" && t.value === "~") {
      eat();
      return wrap64(~parseUnary());
    }
    return parsePrimary();
  };

  const parsePrimary = (): bigint => {
    const t = eat();
    if (t.kind === "num") return wrap64(t.value);
    if (t.kind === "lparen") {
      const value = parseOr();
      const close = eat();
      if (close.kind !== "rparen") throw new Error("缺少右括号");
      return value;
    }
    throw new Error("表达式不完整");
  };

  const result = parseOr();
  if (pos < tokens.length) throw new Error("表达式有多余内容");
  return result;
}

/** 把有符号 64 位整数展开成二进制串（64 位，补码）。 */
function toBinary(v: bigint): string {
  return BigInt.asUintN(64, v).toString(2).padStart(64, "0");
}

/** 每 4 位加一个空格，方便阅读 64 位二进制。 */
function groupBinary(bits: string): string {
  return bits.replace(/(.{4})(?=.)/gu, "$1 ");
}

export const factory: ToolFactory = (ctx) => {
  const exprInput = textInput("例如 (0xFF & 0x0F) << 4 或 2 ** 32", { mono: true });
  const error = errorLine();
  const statLine = stats();

  const decSigned = textInput("", { mono: true });
  const decUnsigned = textInput("", { mono: true });
  const hexOut = textInput("", { mono: true });
  const octOut = textInput("", { mono: true });
  const binOut = h("textarea", {
    class: "input mono",
    attrs: { rows: "3", readonly: "readonly", spellcheck: "false" },
  }) as HTMLTextAreaElement;

  for (const el of [decSigned, decUnsigned, hexOut, octOut]) el.readOnly = true;

  const compute = (): void => {
    const expr = exprInput.value.trim();
    if (expr === "") {
      for (const el of [decSigned, decUnsigned, hexOut, octOut]) el.value = "";
      binOut.value = "";
      showError(error, "");
      setStats(statLine, []);
      return;
    }

    let value: bigint;
    try {
      value = evaluate(expr);
    } catch (e) {
      showError(error, e instanceof Error ? e.message : String(e));
      for (const el of [decSigned, decUnsigned, hexOut, octOut]) el.value = "";
      binOut.value = "";
      setStats(statLine, []);
      return;
    }

    showError(error, "");
    const unsigned = BigInt.asUintN(64, value);
    decSigned.value = value.toString(10);
    decUnsigned.value = unsigned.toString(10);
    hexOut.value = `0x${unsigned.toString(16).toUpperCase()}`;
    octOut.value = `0o${unsigned.toString(8)}`;
    binOut.value = groupBinary(toBinary(value));

    // 顺带给出常见位宽下的截断值，粘贴给 int32 参数时用得上。
    setStats(statLine, [
      ["int32", String(BigInt.asIntN(32, value))],
      ["uint32", String(BigInt.asUintN(32, value))],
      ["int16", String(BigInt.asIntN(16, value))],
      ["uint8", String(BigInt.asUintN(8, value))],
    ]);
  };

  exprInput.addEventListener("input", compute);

  const insert = (text: string): void => {
    const start = exprInput.selectionStart ?? exprInput.value.length;
    const end = exprInput.selectionEnd ?? start;
    exprInput.value = exprInput.value.slice(0, start) + text + exprInput.value.slice(end);
    exprInput.selectionStart = exprInput.selectionEnd = start + text.length;
    exprInput.focus();
    compute();
  };

  // 快捷键盘：位运算与括号是最常用的，逐个手打很烦。
  const keypad = h("div", { class: "calc-keypad" });
  for (const key of [
    "&",
    "|",
    "^",
    "~",
    "<<",
    ">>",
    "(",
    ")",
    "0xFF",
    "0x0F",
    "2 ** 32",
  ]) {
    const btn = h("button", { class: "btn btn--sm mono", attrs: { type: "button" } }, key);
    btn.addEventListener("click", () => insert(key));
    keypad.append(btn);
  }

  const element = h(
    "div",
    { class: "tool-body" },
    field("表达式", exprInput, "支持 + - * / % ** & | ^ ~ << >> 与括号，64 位截断"),
    keypad,
    actions(
      button(
        "计算",
        () => {
          compute();
        },
        "primary",
      ),
      button("清空", () => {
        exprInput.value = "";
        compute();
        exprInput.focus();
      }),
      copyButton(() => hexOut.value, ctx.toast, "复制 HEX"),
      copyButton(() => decUnsigned.value, ctx.toast, "复制无符号"),
    ),
    error,
    h(
      "div",
      { class: "tool-grid-2" },
      field("有符号 DEC", decSigned),
      field("无符号 DEC", decUnsigned),
      field("HEX", hexOut),
      field("OCT", octOut),
    ),
    field("BIN", binOut),
    statLine,
  );

  compute();
  return { element };
};

export default factory;

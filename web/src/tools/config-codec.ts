// 配置格式编解码：YAML 子集 / JSON / INI。
//
// 与 UI 分离：这里全是纯函数，可以单独测试，也不依赖 DOM。
//
// 三种格式的语义边界（决定了哪些转换会有损）：
//   1. JSON 有明确的类型。
//   2. YAML 的标量默认按字符串理解，除非形态像数字 / 布尔 / null。
//   3. INI 只有字符串，且只有两级（section → key）。层级更深或
//      遇到数组时无法无损表达，这里选择报错而不是静默压平。

// ---------------------------------------------------------------------------
// 类型
// ---------------------------------------------------------------------------

type Scalar = string | number | boolean | null;
type Value = Scalar | Value[] | { [key: string]: Value };

class ParseError extends Error {
  constructor(
    message: string,
    readonly line: number,
  ) {
    super(`第 ${line} 行：${message}`);
  }
}

// ---------------------------------------------------------------------------
// YAML 子集解析器
// ---------------------------------------------------------------------------

interface YLine {
  indent: number;
  text: string;
  line: number;
}

/** 去掉注释。引号内的 # 不算注释。 */
function stripComment(raw: string): string {
  let inSingle = false;
  let inDouble = false;
  for (let i = 0; i < raw.length; i += 1) {
    const ch = raw[i];
    if (ch === "'" && !inDouble) inSingle = !inSingle;
    else if (ch === '"' && !inSingle) inDouble = !inDouble;
    else if (ch === "#" && !inSingle && !inDouble) {
      // YAML 要求 # 前有空白才算注释；但行首的 # 也是。
      if (i === 0 || /\s/u.test(raw[i - 1] ?? "")) return raw.slice(0, i);
    }
  }
  return raw;
}

/** 解析标量：引号字符串、数字、布尔、null，其余按原文字符串。 */
function parseScalar(raw: string): Scalar {
  const s = raw.trim();
  if (s === "") return null;

  if (s.startsWith('"') && s.endsWith('"') && s.length >= 2) {
    return s.slice(1, -1).replace(/\\n/gu, "\n").replace(/\\t/gu, "\t").replace(/\\"/gu, '"');
  }
  if (s.startsWith("'") && s.endsWith("'") && s.length >= 2) {
    // 单引号内 '' 表示一个 '
    return s.slice(1, -1).replace(/''/gu, "'");
  }
  if (s === "null" || s === "~") return null;
  if (s === "true" || s === "True" || s === "TRUE") return true;
  if (s === "false" || s === "False" || s === "FALSE") return false;

  // 数字要严格匹配，避免把 "1.2.3" 或 "012" 当数字。
  if (/^-?\d+$/u.test(s)) {
    const n = Number(s);
    if (Number.isSafeInteger(n)) return n;
    return s; // 超出安全范围保留为字符串，避免精度丢失
  }
  if (/^-?\d+\.\d+$/u.test(s)) return Number(s);

  return s;
}

function parseYAML(text: string): Value {
  const lines: YLine[] = [];

  for (const [index, rawLine] of text.split("\n").entries()) {
    const lineNo = index + 1;
    if (/^\s*$/u.test(rawLine)) continue;
    const withoutComment = stripComment(rawLine);
    if (/^\s*$/u.test(withoutComment)) continue;
    if (/^\s*(---|\.\.\.)\s*$/u.test(withoutComment)) continue; // 文档分隔符

    const indentMatch = /^( *)/u.exec(withoutComment);
    const indent = indentMatch ? indentMatch[1]?.length ?? 0 : 0;
    if (/^\t/u.test(withoutComment)) {
      throw new ParseError("缩进不能用 Tab，YAML 只允许空格", lineNo);
    }
    lines.push({ indent, text: withoutComment.trim(), line: lineNo });
  }

  if (lines.length === 0) return null;

  let cursor = 0;

  function parseBlock(): Value {
    const first = lines[cursor];
    if (!first) return null;

    // 序列：同一缩进上以 "- " 开头
    if (first.text === "-" || first.text.startsWith("- ")) {
      return parseSequence(first.indent);
    }
    return parseMapping(first.indent);
  }

  function parseSequence(indent: number): Value[] {
    const out: Value[] = [];
    while (cursor < lines.length) {
      const line = lines[cursor];
      if (!line || line.indent !== indent) break;
      if (!(line.text === "-" || line.text.startsWith("- "))) break;

      const rest = line.text.slice(1).trim();
      cursor += 1;

      if (rest === "") {
        // "- " 后面换行，下一层是嵌套结构
        const next = lines[cursor];
        if (next && next.indent > indent) out.push(parseBlock());
        else out.push(null);
        continue;
      }

      // "- key: value" —— 行内对象起始。把剩余部分当成缩进更深的映射首行。
      if (/^[^:]+:(\s|$)/u.test(rest) && !rest.startsWith('"') && !rest.startsWith("'")) {
        const colon = rest.indexOf(":");
        const key = rest.slice(0, colon).trim();
        const valuePart = rest.slice(colon + 1).trim();

        const obj: { [key: string]: Value } = {};
        if (valuePart === "") {
          const next = lines[cursor];
          obj[key] = next && next.indent > indent ? parseBlock() : null;
        } else {
          obj[key] = parseScalar(valuePart);
        }

        // 同一项的后续键缩进比 indent 深，继续并入这个对象。
        while (cursor < lines.length) {
          const follow = lines[cursor];
          if (!follow || follow.indent <= indent) break;
          if (follow.text.startsWith("- ")) break;
          const m = /^([^:]+):(\s(.*))?$/u.exec(follow.text);
          if (!m) break;
          const k = (m[1] ?? "").trim();
          const v = (m[3] ?? "").trim();
          cursor += 1;
          if (v === "") {
            const deeper = lines[cursor];
            obj[k] = deeper && deeper.indent > follow.indent ? parseBlock() : null;
          } else {
            obj[k] = parseScalar(v);
          }
        }
        out.push(obj);
        continue;
      }

      out.push(parseScalar(rest));
    }
    return out;
  }

  function parseMapping(indent: number): { [key: string]: Value } {
    const out: { [key: string]: Value } = {};
    while (cursor < lines.length) {
      const line = lines[cursor];
      if (!line) break;
      if (line.indent < indent) break;
      if (line.indent > indent) {
        throw new ParseError("缩进不一致：这一行比上一层的键更深，但前面没有对应的键", line.line);
      }
      if (line.text.startsWith("- ")) break;

      const m = /^([^:]+?):(\s+(.*))?$/u.exec(line.text);
      if (!m) {
        throw new ParseError(`无法解析这一行（期望 "key: value"）`, line.line);
      }

      const key = (m[1] ?? "").trim();
      const hasValue = m[2] !== undefined;
      const rawValue = (m[3] ?? "").trim();
      cursor += 1;

      if (!hasValue || rawValue === "") {
        const next = lines[cursor];
        if (next && next.indent > indent) {
          out[key] = parseBlock();
        } else if (next && next.indent === indent && (next.text === "-" || next.text.startsWith("- "))) {
          // 序列可以与父键同缩进（YAML 允许）
          out[key] = parseSequence(indent);
        } else {
          out[key] = null;
        }
        continue;
      }

      // 块标量：| 保留换行，> 折叠为空格
      if (rawValue === "|" || rawValue === ">" || rawValue === "|-" || rawValue === ">-") {
        const collected: string[] = [];
        const blockIndent = lines[cursor]?.indent ?? indent + 2;
        while (cursor < lines.length) {
          const l = lines[cursor];
          if (!l || l.indent < blockIndent) break;
          collected.push(l.text);
          cursor += 1;
        }
        out[key] = rawValue.startsWith("|") ? collected.join("\n") : collected.join(" ");
        continue;
      }

      out[key] = parseScalar(rawValue);
    }
    return out;
  }

  const result = parseBlock();
  if (cursor < lines.length) {
    throw new ParseError("存在无法解析的内容（缩进或结构有误）", lines[cursor]?.line ?? 0);
  }
  return result;
}

// ---------------------------------------------------------------------------
// YAML 序列化
// ---------------------------------------------------------------------------

/** 判断标量是否需要加引号。 */
function needsQuote(s: string): boolean {
  if (s === "") return true;
  if (/^[\s]|[\s]$/u.test(s)) return true;
  if (/[:#\-?*&!|>'"%@`{}[\],]/u.test(s)) return true;
  if (/^(true|false|null|~|yes|no|on|off)$/iu.test(s)) return true;
  if (/^-?\d+(\.\d+)?$/u.test(s)) return true;
  return false;
}

function scalarToYAML(v: Scalar): string {
  if (v === null) return "null";
  if (typeof v === "boolean") return String(v);
  if (typeof v === "number") return String(v);
  if (v.includes("\n")) {
    const indented = v
      .split("\n")
      .map((l) => (l === "" ? "" : `  ${l}`))
      .join("\n");
    return `|\n${indented}`;
  }
  return needsQuote(v) ? JSON.stringify(v) : v;
}

function stringifyYAML(value: Value, depth = 0): string {
  const pad = "  ".repeat(depth);

  if (Array.isArray(value)) {
    if (value.length === 0) return "[]";
    return value
      .map((item) => {
        const inner = stringifyYAML(item, depth + 1);
        if (item !== null && typeof item === "object" && !Array.isArray(item)) {
          // 对象作为序列项时，第一行紧跟 "- "，其余行缩进对齐
          const lines = inner.split("\n").filter((l) => l.trim() !== "");
          const [first, ...rest] = lines;
          return `${pad}- ${first?.trimStart() ?? ""}${
            rest.length ? "\n" + rest.map((l) => "  " + l).join("\n") : ""
          }`;
        }
        if (Array.isArray(item)) return `${pad}- \n${inner}`;
        return `${pad}- ${scalarToYAML(item)}`;
      })
      .join("\n");
  }

  if (value !== null && typeof value === "object") {
    const keys = Object.keys(value);
    if (keys.length === 0) return "{}";
    return keys
      .map((key) => {
        const child = value[key];
        if (child !== null && typeof child === "object" && !Array.isArray(child)) {
          const inner = stringifyYAML(child, depth + 1);
          return `${pad}${key}:\n${inner}`;
        }
        if (Array.isArray(child)) {
          if (child.length === 0) return `${pad}${key}: []`;
          return `${pad}${key}:\n${stringifyYAML(child, depth + 1)}`;
        }
        return `${pad}${key}: ${scalarToYAML(child ?? null)}`;
      })
      .join("\n");
  }

  return `${pad}${scalarToYAML(value)}`;
}

// ---------------------------------------------------------------------------
// INI
// ---------------------------------------------------------------------------

function parseINI(text: string): Value {
  const result: { [key: string]: Value } = {};
  let current: { [key: string]: Value } = result;
  let currentName = "";

  for (const [index, rawLine] of text.split("\n").entries()) {
    const line = rawLine.trim();
    if (line === "" || line.startsWith(";") || line.startsWith("#")) continue;

    const sectionMatch = /^\[(.+)\]$/u.exec(line);
    if (sectionMatch) {
      currentName = sectionMatch[1]?.trim() ?? "";
      const existing = result[currentName];
      if (existing !== null && typeof existing === "object" && !Array.isArray(existing)) {
        current = existing;
      } else {
        current = {};
        result[currentName] = current;
      }
      continue;
    }

    const kv = /^([^=]+)=(.*)$/u.exec(line);
    if (!kv) {
      throw new ParseError("无法解析这一行（期望 key=value 或 [section]）", index + 1);
    }
    const key = (kv[1] ?? "").trim();
    const rawValue = (kv[2] ?? "").trim();
    if (key === "") throw new ParseError("键不能为空", index + 1);
    current[key] = parseScalar(rawValue);
  }

  return result;
}

function stringifyINI(value: Value): string {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error("INI 顶层必须是一个对象");
  }

  const rootLines: string[] = [];
  const sections: string[] = [];

  for (const [key, child] of Object.entries(value)) {
    if (child !== null && typeof child === "object" && !Array.isArray(child)) {
      const inner = Object.entries(child);
      sections.push(
        `[${key}]\n` +
          inner
            .map(([k, v]) => {
              if (v !== null && typeof v === "object") {
                throw new Error(
                  `INI 只有两级：section「${key}」下的「${k}」还是对象，无法表达`,
                );
              }
              return `${k}=${v === null ? "" : String(v)}`;
            })
            .join("\n"),
      );
    } else if (Array.isArray(child)) {
      throw new Error(`INI 不支持数组：键「${key}」的值是数组`);
    } else {
      rootLines.push(`${key}=${child === null ? "" : String(child)}`);
    }
  }

  const parts: string[] = [];
  if (rootLines.length > 0) parts.push(rootLines.join("\n"));
  parts.push(...sections);
  return parts.join("\n\n");
}

// ---------------------------------------------------------------------------
// 格式识别
// ---------------------------------------------------------------------------

function detectFormat(text: string): "json" | "yaml" | "ini" {
  const trimmed = text.trim();
  if (/^[[{]/u.test(trimmed)) return "json";
  if (/^\s*\[[^\]]+\]\s*$/mu.test(trimmed) || /^[A-Za-z_][\w.-]*\s*=/mu.test(trimmed)) {
    // 有 [section] 或 key=value 形式，优先按 INI
    if (!/:(\s|$)/mu.test(trimmed)) return "ini";
  }
  return "yaml";
}

function parseAny(text: string, format: "json" | "yaml" | "ini"): Value {
  if (format === "json") {
    try {
      return JSON.parse(text) as Value;
    } catch (e) {
      const message = e instanceof Error ? e.message : String(e);
      const pos = /position (\d+)/u.exec(message);
      if (pos) {
        const before = text.slice(0, Number(pos[1]));
        throw new Error(`JSON 语法错误：第 ${before.split("\n").length} 行，${message}`);
      }
      throw new Error(`JSON 语法错误：${message}`);
    }
  }
  if (format === "ini") return parseINI(text);
  return parseYAML(text);
}

function serialize(value: Value, format: "json" | "yaml" | "ini"): string {
  if (format === "json") return JSON.stringify(value, null, 2);
  if (format === "ini") return stringifyINI(value);
  return stringifyYAML(value);
}


export type Format = "json" | "yaml" | "ini";

export { detectFormat, parseAny, serialize };

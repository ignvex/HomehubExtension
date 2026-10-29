// SSE 客户端。
//
// 用 EventSource 而不是 WebSocket：服务端只做单向推送，EventSource 自带
// 断线重连与 Last-Event-ID 补发，省掉一整套重连状态机。
//
// 认证靠 hh_session cookie（HttpOnly，同源自动携带）。EventSource 不支持
// 自定义请求头，所以这里不能用 Bearer。

export type ServerEventType =
  | "entry.created"
  | "feed.updated"
  | "job.finished"
  | "system.alert"
  | "system.resync";

export interface ServerEvent {
  type: ServerEventType | string;
  data: unknown;
  id: number;
}

type Listener = (event: ServerEvent) => void;

const STREAM_PATH = "/api/v1/events/stream";
const HIDDEN_CLOSE_MS = 5 * 60 * 1000;
const ERROR_BACKOFF_MS = 3000;

let source: EventSource | null = null;
const listeners = new Set<Listener>();
let hiddenTimer: number | null = null;
let started = false;

function emit(event: ServerEvent): void {
  for (const listener of listeners) {
    try {
      listener(event);
    } catch (error) {
      // 单个订阅者抛错不能拖垮连接。
      console.error("SSE listener failed", error);
    }
  }
}

function parse(event: MessageEvent<string>, type: string): ServerEvent {
  const raw = event as MessageEvent<string> & { lastEventId?: string };
  let data: unknown = null;
  try {
    data = JSON.parse(event.data);
  } catch {
    data = event.data;
  }
  return {
    type,
    data,
    id: Number(raw.lastEventId ?? "0") || 0,
  };
}

function connect(): void {
  if (source !== null || listeners.size === 0) return;
  if (document.visibilityState === "hidden") return;

  const es = new EventSource(STREAM_PATH, { withCredentials: true });
  source = es;

  // 事件类型必须在服务端事先声明过的白名单里逐个注册：
  // EventSource 的 onmessage 只接收没有 event: 字段的默认消息。
  const types: ServerEventType[] = [
    "entry.created",
    "feed.updated",
    "job.finished",
    "system.alert",
    "system.resync",
  ];
  for (const type of types) {
    es.addEventListener(type, (event) => {
      emit(parse(event as MessageEvent<string>, type));
    });
  }

  es.addEventListener("open", () => {
    // 重连后让调用方重新拉一次列表：断线期间的事件收不到，
    // 服务端只在 5 分钟窗口内补发。
    emit({ type: "system.resync", data: null, id: 0 });
  });

  es.addEventListener("error", () => {
    // EventSource 会自己重连；这里只在彻底 CLOSED 时手动兜底，
    // 避免浏览器放弃重连后页面永远收不到更新。
    if (es.readyState === EventSource.CLOSED) {
      es.close();
      source = null;
      window.setTimeout(() => connect(), ERROR_BACKOFF_MS);
    }
  });
}

function disconnect(): void {
  if (source) {
    source.close();
    source = null;
  }
}

/** 订阅事件。返回取消函数，视图 unmount 时必须调用。 */
export function subscribe(listener: Listener): () => void {
  listeners.add(listener);
  connect();
  return () => {
    listeners.delete(listener);
    if (listeners.size === 0) disconnect();
  };
}

/** 在 App 启动时调用一次，处理页面可见性导致的连接启停。 */
export function initEvents(): void {
  if (started) return;
  started = true;

  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState === "hidden") {
      // 后台标签页不该长期占着一个连接。
      // 但立刻关会在切标签时频繁重连，所以延时关。
      hiddenTimer = window.setTimeout(() => disconnect(), HIDDEN_CLOSE_MS);
      return;
    }
    if (hiddenTimer !== null) {
      window.clearTimeout(hiddenTimer);
      hiddenTimer = null;
    }
    connect();
  });

  window.addEventListener("online", () => connect());
  window.addEventListener("offline", () => disconnect());
}

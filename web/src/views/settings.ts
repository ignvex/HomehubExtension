import { getJobs, getStats, getSystemInfo } from "../api/system";
import type { JobRun, SystemInfo, SystemStats } from "../api/types";
import { forgetDevice, getSession, loginWithDevice } from "../auth/session";
import { loadDeviceKey } from "../auth/storage";
import { pageHeader, section } from "../components/page";
import { errorState, loadingBlock } from "../components/states";
import { icon } from "../components/icons";
import { h } from "../lib/dom";
import {
  formatBytes,
  formatCount,
  formatDateTime,
  formatDuration,
  formatInstantString,
  parseTime,
} from "../lib/format";
import { describeError, jobStatusLabel, jobStatusTone } from "../lib/strings";
import { applyTheme, currentTheme, THEME_LABELS, THEMES } from "../lib/theme";
import type { ViewFactory } from "../router";

function detailRow(label: string, value: Node | string, mono = false): HTMLElement {
  return h(
    "div",
    { class: "detail-row" },
    h("dt", {}, label),
    h("dd", { class: mono ? "mono break-all" : "" }, value),
  );
}

function metric(label: string, value: string, hint?: string): HTMLElement {
  return h(
    "div",
    { class: "metric" },
    h("span", { class: "metric-value" }, value),
    h("span", { class: "metric-label" }, label),
    hint ? h("span", { class: "metric-hint" }, hint) : null,
  );
}

function renderInfo(info: SystemInfo): HTMLElement {
  return h(
    "dl",
    { class: "detail-list" },
    detailRow("版本", info.version || "开发版", true),
    detailRow("提交", info.commit || "—", true),
    detailRow("构建时间", formatInstantString(info.build_time)),
    detailRow("Go 版本", info.go_version || "—", true),
    detailRow("Schema 版本", String(info.schema_version)),
    detailRow("运行时长", formatDuration(info.uptime_seconds)),
    info.public_url ? detailRow("公开地址", info.public_url, true) : null,
  );
}

function renderStats(stats: SystemStats): HTMLElement {
  const hasDisk = typeof stats.disk_total === "number" && typeof stats.disk_free === "number";
  return h(
    "div",
    { class: "metric-grid" },
    metric("片段", formatCount(stats.pastes)),
    metric("订阅源", formatCount(stats.feeds)),
    metric("条目", formatCount(stats.entries)),
    metric("未读", formatCount(stats.unread)),
    metric("媒体文件", formatCount(stats.blobs)),
    metric("媒体占用", formatBytes(stats.blob_bytes)),
    metric("待处理媒体", formatCount(stats.pending_media)),
    hasDisk
      ? metric("磁盘可用", formatBytes(stats.disk_free), `共 ${formatBytes(stats.disk_total)}`)
      : null,
  );
}

function renderJobs(jobs: readonly JobRun[]): HTMLElement {
  if (jobs.length === 0) {
    return h(
      "p",
      { class: "muted" },
      "还没有任务记录。清理任务按配置的 cron 调度运行，执行结果会记录在这里。",
    );
  }

  const rows = jobs.map((job) => {
    const start = parseTime(job.started_at);
    const end = parseTime(job.finished_at);
    const duration = start && end ? formatDuration((end.getTime() - start.getTime()) / 1000) : "—";
    return h(
      "tr",
      {},
      h("td", { class: "mono" }, job.job),
      h(
        "td",
        {},
        h(
          "span",
          { class: `status status--${jobStatusTone(job.status)}` },
          h("span", { class: "status-dot", attrs: { "aria-hidden": "true" } }),
          jobStatusLabel(job.status),
        ),
      ),
      h("td", { class: "nowrap" }, formatDateTime(job.started_at)),
      h("td", { class: "nowrap" }, job.finished_at ? formatDateTime(job.finished_at) : "运行中"),
      h("td", { class: "nowrap" }, duration),
      h("td", { class: "job-detail" }, job.detail.trim() !== "" ? job.detail : "—"),
    );
  });

  return h(
    "div",
    { class: "table-wrap" },
    h(
      "table",
      { class: "table" },
      h("caption", { class: "visually-hidden" }, "最近 50 条清理任务记录"),
      h(
        "thead",
        {},
        h(
          "tr",
          {},
          h("th", { attrs: { scope: "col" } }, "任务"),
          h("th", { attrs: { scope: "col" } }, "状态"),
          h("th", { attrs: { scope: "col" } }, "开始"),
          h("th", { attrs: { scope: "col" } }, "结束"),
          h("th", { attrs: { scope: "col" } }, "耗时"),
          h("th", { attrs: { scope: "col" } }, "详情"),
        ),
      ),
      h("tbody", {}, ...rows),
    ),
  );
}

export const settingsView: ViewFactory = (ctx) => {
  const infoHost = h("div", {}, loadingBlock());
  const statsHost = h("div", {}, loadingBlock());
  const jobsHost = h("div", {}, loadingBlock());
  const refresh = h(
    "button",
    { class: "btn", attrs: { type: "button" } },
    icon("refresh"),
    "刷新",
  );

  const loadInfo = async (): Promise<void> => {
    infoHost.replaceChildren(loadingBlock());
    try {
      const info = await getSystemInfo(ctx.signal);
      if (ctx.signal.aborted) return;
      infoHost.replaceChildren(renderInfo(info));
    } catch (error) {
      if (ctx.signal.aborted) return;
      infoHost.replaceChildren(
        errorState(describeError(error), () => {
          void loadInfo();
        }),
      );
    }
  };

  const loadStats = async (): Promise<void> => {
    statsHost.replaceChildren(loadingBlock());
    try {
      const stats = await getStats(ctx.signal);
      if (ctx.signal.aborted) return;
      statsHost.replaceChildren(renderStats(stats));
    } catch (error) {
      if (ctx.signal.aborted) return;
      statsHost.replaceChildren(
        errorState(describeError(error), () => {
          void loadStats();
        }),
      );
    }
  };

  const loadJobs = async (): Promise<void> => {
    jobsHost.replaceChildren(loadingBlock());
    try {
      const jobs = await getJobs(ctx.signal);
      if (ctx.signal.aborted) return;
      jobsHost.replaceChildren(renderJobs(jobs));
    } catch (error) {
      if (ctx.signal.aborted) return;
      jobsHost.replaceChildren(
        errorState(describeError(error), () => {
          void loadJobs();
        }),
      );
    }
  };

  const themeField = h("fieldset", {
    class: "segmented",
    attrs: { "aria-label": "界面主题" },
  });
  const themeLabels: Array<{ theme: (typeof THEMES)[number]; label: HTMLElement; input: HTMLInputElement }> = [];
  const activeTheme = currentTheme();
  for (const theme of THEMES) {
    const input = h("input", {
      attrs: {
        type: "radio",
        name: "theme",
        value: theme,
        ...(theme === activeTheme ? { checked: true } : {}),
      },
    });
    const iconName = theme === "auto" ? "monitor" : theme === "light" ? "sun" : "moon";
    const label = h(
      "label",
      { class: `segment${theme === activeTheme ? " is-active" : ""}` },
      input,
      icon(iconName),
      h("span", {}, THEME_LABELS[theme]),
    );
    themeLabels.push({ theme, label, input });
    input.addEventListener("change", () => {
      if (!input.checked) return;
      applyTheme(theme);
      for (const item of themeLabels) item.label.classList.toggle("is-active", item.input.checked);
      window.dispatchEvent(new Event("hh:theme"));
      ctx.toast.show(`主题：${THEME_LABELS[theme]}`, "success");
    });
    themeField.append(label);
  }

  const deviceBody = h("div", { class: "device-panel" });
  const client = getSession().client;
  const keyHost = h("div", { class: "device-key" }, loadingBlock("正在读取本机密钥…"));

  const relogin = h(
    "button",
    { class: "btn", attrs: { type: "button" } },
    icon("refresh"),
    "重新登录",
  );
  relogin.addEventListener("click", () => {
    relogin.disabled = true;
    void loginWithDevice(ctx.signal)
      .then(() => ctx.toast.show("已重新登录", "success"))
      .catch((error: unknown) => {
        if (!ctx.signal.aborted) ctx.toast.show(describeError(error), "error");
      })
      .finally(() => {
        relogin.disabled = false;
      });
  });

  const forget = h(
    "button",
    { class: "btn btn--danger", attrs: { type: "button" } },
    icon("trash"),
    "清除本机设备",
  );
  let armed = false;
  let timer: number | null = null;
  const resetForget = (): void => {
    armed = false;
    forget.replaceChildren(icon("trash"), "清除本机设备");
    if (timer !== null) {
      window.clearTimeout(timer);
      timer = null;
    }
  };
  forget.addEventListener("click", () => {
    if (!armed) {
      armed = true;
      forget.replaceChildren(icon("alert"), "确认清除本机密钥");
      timer = window.setTimeout(resetForget, 4000);
      return;
    }
    resetForget();
    forget.disabled = true;
    void forgetDevice()
      .then(() => {
        ctx.toast.show("已清除本机设备", "success");
        ctx.router.navigate("/pair", { replace: true });
      })
      .catch((error: unknown) => {
        ctx.toast.show(describeError(error), "error");
        forget.disabled = false;
      });
  });

  deviceBody.append(
    h(
      "dl",
      { class: "detail-list" },
      detailRow("设备名称", client?.name ?? "未登录"),
      detailRow("设备 ID", client?.id ?? "—", true),
    ),
    keyHost,
    h("div", { class: "form-actions" }, relogin, forget),
    h(
      "p",
      { class: "muted small" },
      "清除只影响本机浏览器中的私钥与 client_id；服务端设备仍然存在。重新配对需要新的配对码。",
    ),
  );

  void loadDeviceKey()
    .then((record) => {
      if (ctx.signal.aborted) return;
      if (!record) {
        keyHost.replaceChildren(
          h(
            "p",
            { class: "notice notice--warning" },
            icon("alert"),
            h("span", {}, "本机没有可用私钥。当前会话仍然有效，但过期后需要重新配对。"),
          ),
        );
        return;
      }
      keyHost.replaceChildren(
        h(
          "p",
          { class: "device-key-line" },
          h("span", {}, "本机密钥"),
          h("span", { class: "badge badge--mono" }, record.algorithm === "ed25519" ? "Ed25519" : "P-256"),
          h("span", { class: "muted small" }, `创建于 ${formatDateTime(record.createdAt)}`),
        ),
        h(
          "p",
          { class: "mono small break-all muted" },
          `公钥 ${record.publicKey.slice(0, 32)}…`,
        ),
      );
    })
    .catch((error: unknown) => {
      if (ctx.signal.aborted) return;
      keyHost.replaceChildren(
        h("p", { class: "notice notice--warning" }, icon("alert"), h("span", {}, describeError(error))),
      );
    });

  const element = h(
    "div",
    { class: "page" },
    pageHeader("设置", "运行状态、存储占用、清理记录与本机设备。", [refresh]),
    section("外观", "主题偏好写入 hh_theme cookie，服务端渲染首帧时直接使用。", themeField),
    section("系统信息", "构建版本与运行时状态。", infoHost),
    section("存储与内容", "数据库计数与媒体文件占用。", statsHost),
    section("清理任务记录", "最近 50 次清理任务的执行结果。", jobsHost),
    section("本机设备", "访问令牌只保存在内存里；私钥不可导出，只在本机使用。", deviceBody),
  );

  refresh.addEventListener("click", () => {
    void loadInfo();
    void loadStats();
    void loadJobs();
  });
  void loadInfo();
  void loadStats();
  void loadJobs();

  return {
    element,
    unmount: () => {
      if (timer !== null) window.clearTimeout(timer);
    },
  };
};

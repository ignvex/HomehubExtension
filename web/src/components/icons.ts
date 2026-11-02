const SVG_NS = "http://www.w3.org/2000/svg";

export type IconName =
  | "home"
  | "inbox"
  | "rss"
  | "paste"
  | "key"
  | "settings"
  | "list"
  | "menu"
  | "close"
  | "sun"
  | "moon"
  | "monitor"
  | "star"
  | "star-filled"
  | "check"
  | "circle"
  | "trash"
  | "copy"
  | "plus"
  | "arrow-left"
  | "external"
  | "refresh"
  | "alert"
  | "clock"
  | "user"
  | "lock"
  | "database"
  | "chevron-right"
  | "filter"
  // 开发工具用
  | "tools"
  | "braces"
  | "regex"
  | "calculator"
  | "binary"
  | "link"
  | "hash"
  | "file-code"
  | "image"
  | "minimize"
  | "image-plus"
  | "fingerprint"
  | "qr"
  | "palette"
  | "type"
  | "search"
  | "upload"
  | "download";

interface IconDefinition {
  paths: readonly string[];
  filled?: boolean;
}

const ICONS: Record<IconName, IconDefinition> = {
  home: { paths: ["M3 10.5 12 3l9 7.5", "M5 9.5V21h14V9.5", "M9.5 21v-6h5v6"] },
  inbox: { paths: ["M4 4h16v16H4z", "M4 13h5l2 3h2l2-3h5"] },
  rss: { paths: ["M4 11a9 9 0 0 1 9 9", "M4 4a16 16 0 0 1 16 16", "M5 19h.01"] },
  paste: { paths: ["M9 4h6v3H9z", "M7 5H5v16h14V5h-2", "M9 12h6", "M9 16h6"] },
  key: {
    paths: [
      "M14.5 5.5a4.5 4.5 0 1 1 3.2 7.66L9 22H4v-5l8.84-8.84A4.48 4.48 0 0 1 14.5 5.5z",
      "M16.5 8.5h.01",
    ],
  },
  settings: {
    paths: [
      "M4 6h16",
      "M4 12h16",
      "M4 18h16",
      "M8 6a1.5 1.5 0 1 0 0-3 1.5 1.5 0 0 0 0 3z",
      "M16 15a1.5 1.5 0 1 0 0-3 1.5 1.5 0 0 0 0 3z",
      "M10 21a1.5 1.5 0 1 0 0-3 1.5 1.5 0 0 0 0 3z",
    ],
  },
  list: { paths: ["M8 6h13", "M8 12h13", "M8 18h13", "M3.5 6h.01", "M3.5 12h.01", "M3.5 18h.01"] },
  menu: { paths: ["M4 7h16", "M4 12h16", "M4 17h16"] },
  close: { paths: ["M6 6l12 12", "M18 6 6 18"] },
  sun: {
    paths: [
      "M12 8a4 4 0 1 0 0 8 4 4 0 0 0 0-8z",
      "M12 2v2",
      "M12 20v2",
      "M4.93 4.93l1.42 1.42",
      "M17.65 17.65l1.42 1.42",
      "M2 12h2",
      "M20 12h2",
      "M4.93 19.07l1.42-1.42",
      "M17.65 6.35l1.42-1.42",
    ],
  },
  moon: { paths: ["M20.5 14.2A8.5 8.5 0 0 1 9.8 3.5 8.5 8.5 0 1 0 20.5 14.2z"] },
  monitor: { paths: ["M3 4h18v12H3z", "M8 20h8", "M12 16v4"] },
  star: {
    paths: ["M12 3.6l2.6 5.27 5.82.85-4.21 4.1.99 5.8L12 16.9l-5.2 2.72.99-5.8-4.21-4.1 5.82-.85z"],
  },
  "star-filled": {
    paths: ["M12 3.6l2.6 5.27 5.82.85-4.21 4.1.99 5.8L12 16.9l-5.2 2.72.99-5.8-4.21-4.1 5.82-.85z"],
    filled: true,
  },
  check: { paths: ["M20 6 9 17l-5-5"] },
  circle: { paths: ["M12 4a8 8 0 1 0 0 16 8 8 0 0 0 0-16z"] },
  trash: { paths: ["M4 7h16", "M9 7V4h6v3", "M6 7l1 14h10l1-14", "M10 11v6", "M14 11v6"] },
  copy: { paths: ["M9 9h11v11H9z", "M15 5H4v11h3"] },
  plus: { paths: ["M12 5v14", "M5 12h14"] },
  "arrow-left": { paths: ["M19 12H5", "m12 19-7-7 7-7"] },
  external: { paths: ["M14 4h6v6", "M20 4 10 14", "M20 14v6H4V4h6"] },
  refresh: { paths: ["M20 12a8 8 0 1 1-2.34-5.66", "M20 4v6h-6"] },
  alert: { paths: ["M12 3 2.5 20h19L12 3z", "M12 9v5", "M12 17.5h.01"] },
  clock: { paths: ["M12 4a8 8 0 1 0 0 16 8 8 0 0 0 0-16z", "M12 8v4l3 2"] },
  user: { paths: ["M12 12a4 4 0 1 0 0-8 4 4 0 0 0 0 8z", "M4 21a8 8 0 0 1 16 0"] },
  lock: { paths: ["M5 11h14v10H5z", "M8 11V7a4 4 0 0 1 8 0v4"] },
  database: { paths: ["M4 6c0-1.66 3.58-3 8-3s8 1.34 8 3-3.58 3-8 3-8-1.34-8-3z", "M4 6v12c0 1.66 3.58 3 8 3s8-1.34 8-3V6", "M4 12c0 1.66 3.58 3 8 3s8-1.34 8-3"] },
  "chevron-right": { paths: ["m9 6 6 6-6 6"] },
  filter: { paths: ["M3 5h18", "M6 12h12", "M10 19h4"] },
  tools: {
    paths: [
      "M14.7 6.3a4 4 0 0 1 5 5l-2.4-2.4-2.6 2.6-2.4-2.4 2.4-2.8z",
      "M12.3 9 4 17.3V20h2.7L15 11.7",
      "M17 14l3 3",
    ],
  },
  braces: { paths: ["M9 4H7a2 2 0 0 0-2 2v4l-2 2 2 2v4a2 2 0 0 0 2 2h2", "M15 4h2a2 2 0 0 1 2 2v4l2 2-2 2v4a2 2 0 0 1-2 2h-2"] },
  regex: { paths: ["M4 18h4", "M6 18v-4", "M14 8v.01", "M18 12v.01", "M10 15v.01", "M16 6v.01"] },
  calculator: {
    paths: ["M5 3h14v18H5z", "M8 7h8v3H8z", "M8 14h.01", "M12 14h.01", "M16 14h.01", "M8 17.5h.01", "M12 17.5h.01", "M16 17.5h.01"],
  },
  binary: { paths: ["M7 5h3v6H7z", "M14 13h3v6h-3z", "M7 17h1", "M16 5h1"] },
  link: { paths: ["M10 13a5 5 0 0 0 7 0l2-2a5 5 0 0 0-7-7l-1 1", "M14 11a5 5 0 0 0-7 0l-2 2a5 5 0 0 0 7 7l1-1"] },
  hash: { paths: ["M5 9h14", "M5 15h14", "M10 3 8 21", "M16 3l-2 18"] },
  "file-code": { paths: ["M6 3h8l4 4v14H6z", "M14 3v4h4", "M9 12l2 2-2 2", "M13 16h2"] },
  image: { paths: ["M4 5h16v14H4z", "M8.5 10a1.5 1.5 0 1 0 0-3 1.5 1.5 0 0 0 0 3z", "M5 17l4.5-4.5 3 3L16 12l3 3"] },
  minimize: { paths: ["M4 9V4h5", "M20 9V4h-5", "M4 15v5h5", "M20 15v5h-5"] },
  "image-plus": { paths: ["M4 5h11v14H4z", "M7.5 10a1.5 1.5 0 1 0 0-3 1.5 1.5 0 0 0 0 3z", "M5 17l3.5-3.5 2.5 2.5L14 13", "M19 8v6", "M16 11h6"] },
  fingerprint: { paths: ["M12 4a8 8 0 0 0-8 8v3", "M12 8a4 4 0 0 0-4 4v5", "M12 12v8", "M16 12a4 4 0 0 1 4-4v7"] },
  qr: { paths: ["M4 4h6v6H4z", "M14 4h6v6h-6z", "M4 14h6v6H4z", "M14 14h2v2h-2z", "M18 14h2v2h-2z", "M14 18h2v2h-2z", "M18 18h2v2h-2z"] },
  palette: { paths: ["M12 3a9 9 0 1 0 0 18h2a2 2 0 0 0 0-4 2 2 0 0 1 2-2h1a4 4 0 0 0 4-4 9 9 0 0 0-9-8z", "M8 9v.01", "M12 7v.01", "M16 9v.01"] },
  type: { paths: ["M4 6V4h16v2", "M12 4v16", "M9 20h6"] },
  search: { paths: ["M11 4a7 7 0 1 0 0 14 7 7 0 0 0 0-14z", "M16 16l4 4"] },
  upload: { paths: ["M4 15v3a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2v-3", "M8 8l4-4 4 4", "M12 4v12"] },
  download: { paths: ["M4 15v3a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2v-3", "M8 11l4 4 4-4", "M12 3v12"] },
};

export function icon(name: IconName, className = "icon"): SVGSVGElement {
  const definition = ICONS[name];
  const svg = document.createElementNS(SVG_NS, "svg");
  svg.setAttribute("viewBox", "0 0 24 24");
  svg.setAttribute("class", className);
  svg.setAttribute("aria-hidden", "true");
  svg.setAttribute("focusable", "false");
  if (definition.filled) {
    svg.setAttribute("fill", "currentColor");
  } else {
    svg.setAttribute("fill", "none");
    svg.setAttribute("stroke", "currentColor");
    svg.setAttribute("stroke-width", "1.75");
    svg.setAttribute("stroke-linecap", "round");
    svg.setAttribute("stroke-linejoin", "round");
  }
  for (const pathData of definition.paths) {
    const path = document.createElementNS(SVG_NS, "path");
    path.setAttribute("d", pathData);
    svg.append(path);
  }
  return svg;
}

const APP_ICONS: Record<string, IconName> = {
  paste: "paste",
  pastebin: "paste",
  rss: "rss",
  feed: "rss",
  feeds: "list",
  list: "list",
  entries: "inbox",
  reader: "inbox",
  settings: "settings",
  system: "settings",
  tools: "tools",
  workbench: "tools",
};

export function appIcon(name: string): SVGSVGElement | null {
  const mapped = APP_ICONS[name.toLowerCase()];
  return mapped ? icon(mapped) : null;
}

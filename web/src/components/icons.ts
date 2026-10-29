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
  | "filter";

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
};

export function appIcon(name: string): SVGSVGElement | null {
  const mapped = APP_ICONS[name.toLowerCase()];
  return mapped ? icon(mapped) : null;
}

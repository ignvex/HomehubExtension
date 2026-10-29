export type Child = Node | string | number | null | undefined | false | readonly Child[];

export interface ElementProps {
  class?: string;
  id?: string;
  title?: string;
  /** 纯文本快捷方式；永远走 textContent。 */
  text?: string;
  dataset?: Record<string, string>;
  attrs?: Record<string, string | number | boolean | null | undefined>;
  on?: Record<string, (event: Event) => void>;
}

export function h<K extends keyof HTMLElementTagNameMap>(
  tag: K,
  props: ElementProps = {},
  ...children: Child[]
): HTMLElementTagNameMap[K] {
  const element = document.createElement(tag);
  if (props.class) element.className = props.class;
  if (props.id) element.id = props.id;
  if (props.title) element.title = props.title;
  if (props.text !== undefined) element.textContent = props.text;

  if (props.dataset) {
    for (const [key, value] of Object.entries(props.dataset)) element.dataset[key] = value;
  }
  if (props.attrs) {
    for (const [key, value] of Object.entries(props.attrs)) {
      if (value === null || value === undefined || value === false) continue;
      element.setAttribute(key, value === true ? "" : String(value));
    }
  }
  if (props.on) {
    for (const [eventName, handler] of Object.entries(props.on)) {
      element.addEventListener(eventName, handler);
    }
  }

  append(element, ...children);
  return element;
}

export function append(parent: Node, ...children: Child[]): void {
  for (const child of children) {
    if (child === null || child === undefined || child === false) continue;
    if (Array.isArray(child)) {
      append(parent, ...child);
      continue;
    }
    if (child instanceof Node) {
      parent.appendChild(child);
      continue;
    }
    parent.appendChild(document.createTextNode(String(child)));
  }
}

export function clear(element: Element): void {
  element.replaceChildren();
}

export function setChildren(element: Element, ...children: Child[]): void {
  element.replaceChildren();
  append(element, ...children);
}

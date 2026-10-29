import "./styles/main.css";
import { mountApp } from "./app";

const root = document.querySelector<HTMLElement>("#app");
if (!root) {
  throw new Error("缺少 #app 挂载点");
}

void mountApp(root).catch((error: unknown) => {
  root.replaceChildren();
  const message = document.createElement("p");
  message.className = "boot-error";
  message.textContent = `前端初始化失败：${error instanceof Error ? error.message : String(error)}`;
  root.append(message);
  console.error(error);
});

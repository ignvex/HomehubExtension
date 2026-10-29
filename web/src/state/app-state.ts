import { getStats } from "../api/system";
import { createStore } from "./store";

export const unreadCount = createStore(0);

/** 顶栏/导航的未读徽标。失败时保持旧值，不打断当前页面。 */
export async function refreshUnreadCount(): Promise<void> {
  try {
    const stats = await getStats();
    unreadCount.set(stats.unread);
  } catch {
    // 徽标是辅助信息，失败静默；页面自身的错误状态会提示。
  }
}

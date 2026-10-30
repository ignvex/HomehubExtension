// 匿名用户的所有权凭证。
//
// 背景：创建片段不需要登录，但「私有片段」和「管理自己的片段」都需要
// 一个身份。服务端在首次匿名创建时生成一个 owner token，之后凭它识别。
//
// 存储位置：localStorage。它不是登录凭证——没有它服务端不会认为你是
// 任何人，只是让你能看到/管理自己创建过的东西。
//
// 代价要说清楚：清站点数据、换浏览器、换设备，token 就没了，
// 那些片段也就无法再管理。这是「不设账号」的固有取舍。

const OWNER_TOKEN_KEY = "hh_owner_token";

export function loadOwnerToken(): string {
  try {
    return localStorage.getItem(OWNER_TOKEN_KEY) ?? "";
  } catch {
    // 隐私模式下 localStorage 可能抛错，降级为无归属。
    return "";
  }
}

export function saveOwnerToken(token: string): void {
  if (!token) return;
  try {
    localStorage.setItem(OWNER_TOKEN_KEY, token);
  } catch {
    // 存不下就只在本页有效，刷新后丢失。
  }
}

export function clearOwnerToken(): void {
  try {
    localStorage.removeItem(OWNER_TOKEN_KEY);
  } catch {
    // 忽略。
  }
}

export function hasOwnerToken(): boolean {
  return loadOwnerToken() !== "";
}

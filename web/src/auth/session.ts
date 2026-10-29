import { requestChallenge, exchangeToken, refreshToken, enrollDevice } from "../api/auth";
import type { AuthClient, AuthSessionResponse, EnrollResponse } from "../api/types";
import { authMessage, generateDeviceKey, signMessage } from "./keys";
import {
  clearClientId,
  deleteDeviceKey,
  getClientId,
  loadDeviceKey,
  saveDeviceKey,
  setClientId,
} from "./storage";

export interface SessionSnapshot {
  authenticated: boolean;
  client: AuthClient | null;
  expiresAt: number | null;
}

let accessToken: string | null = null;
let csrfToken: string | null = null;
let client: AuthClient | null = null;
let expiresAt: number | null = null;
let pendingRefresh: Promise<boolean> | null = null;
let snapshot: SessionSnapshot = { authenticated: false, client: null, expiresAt: null };
const listeners = new Set<(state: SessionSnapshot) => void>();

function emit(): void {
  snapshot = {
    authenticated: accessToken !== null && client !== null,
    client: client ? { ...client } : null,
    expiresAt,
  };
  for (const listener of listeners) listener(snapshot);
}

function applySession(response: AuthSessionResponse): void {
  accessToken = response.access_token;
  csrfToken = response.csrf_token;
  client = { id: response.client.id, name: response.client.name };
  expiresAt = Date.now() + response.expires_in * 1000;
  setClientId(response.client.id);
  emit();
}

export function getAccessToken(): string | null {
  return accessToken;
}

export function getCsrfToken(): string | null {
  return csrfToken;
}

export function getSession(): SessionSnapshot {
  return snapshot;
}

export function isAuthenticated(): boolean {
  return snapshot.authenticated;
}

export function subscribeSession(listener: (state: SessionSnapshot) => void): () => void {
  listeners.add(listener);
  listener(snapshot);
  return () => listeners.delete(listener);
}

export function clearSession(): void {
  accessToken = null;
  csrfToken = null;
  client = null;
  expiresAt = null;
  emit();
}

export function hasLocalClientId(): boolean {
  return getClientId() !== null;
}

/**
 * 页面加载时恢复会话：refresh_token 在 HttpOnly cookie 里，前端拿不到也不需要读。
 * 一次 refresh 同时拿回 access token、csrf 与 client，不需要再调 /auth/me。
 */
export async function restoreSession(): Promise<boolean> {
  if (!hasLocalClientId()) return false;
  try {
    applySession(await refreshToken());
    return true;
  } catch {
    clearSession();
    return false;
  }
}

/**
 * 共享同一个刷新 Promise：并发 401 只触发一次 refresh，
 * 避免旋转 refresh token 时互相把对方的会话作废。
 */
export function refreshSession(): Promise<boolean> {
  if (pendingRefresh) return pendingRefresh;

  pendingRefresh = (async () => {
    try {
      applySession(await refreshToken());
      return true;
    } catch {
      clearSession();
      return false;
    } finally {
      pendingRefresh = null;
    }
  })();

  return pendingRefresh;
}

/** challenge → 签名 → 换 token。配对成功后立即调用一次，验证密钥可用。 */
export async function loginWithDevice(signal?: AbortSignal): Promise<AuthClient> {
  const clientId = getClientId();
  if (!clientId) throw new Error("本设备尚未配对，请先使用配对码登记");

  const record = await loadDeviceKey();
  if (!record) throw new Error("本机私钥不存在，请重新配对");

  const challenge = await requestChallenge(clientId, signal);
  const signature = await signMessage(
    record.privateKey,
    record.algorithm,
    authMessage(clientId, challenge.nonce),
  );
  const response = await exchangeToken(
    { client_id: clientId, nonce: challenge.nonce, signature },
    signal,
  );
  applySession(response);
  return response.client;
}

/**
 * 生成密钥 → 登记公钥 → 保存 client_id → 立即走一次登录。
 *
 * 只有服务端登记成功后才写 IndexedDB，避免本地留下一个服务端不认识的密钥。
 */
export async function enrollThisDevice(
  token: string,
  name: string,
  signal?: AbortSignal,
): Promise<EnrollResponse> {
  const generated = await generateDeviceKey();
  const response = await enrollDevice(
    {
      token,
      name,
      algorithm: generated.algorithm,
      public_key: generated.publicKey,
    },
    signal,
  );

  await saveDeviceKey({
    privateKey: generated.privateKey,
    publicKey: generated.publicKey,
    algorithm: generated.algorithm,
    createdAt: Math.floor(Date.now() / 1000),
  });
  setClientId(response.client_id);

  await loginWithDevice(signal);
  return response;
}

/** 清除本机密钥与本地 client_id；服务端设备仍然存在，可再用配对码登记新密钥。 */
export async function forgetDevice(): Promise<void> {
  clearSession();
  try {
    await deleteDeviceKey();
  } finally {
    clearClientId();
  }
}

/** 仅供配对页判断：本机是否已有可用私钥。 */
export async function hasLocalDeviceKey(): Promise<boolean> {
  try {
    return (await loadDeviceKey()) !== null;
  } catch {
    return false;
  }
}

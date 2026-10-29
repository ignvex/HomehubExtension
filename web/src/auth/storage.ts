import type { AuthAlgorithm } from "../api/types";

const DB_NAME = "homehubext";
const DB_VERSION = 1;
const STORE_NAME = "device";
const RECORD_KEY = "device-key";
const CLIENT_ID_KEY = "hh_client_id";

export interface DeviceKeyRecord {
  privateKey: CryptoKey;
  publicKey: string;
  algorithm: AuthAlgorithm;
  /** Unix 秒。与 API 的时刻单位保持一致，避免两套换算。 */
  createdAt: number;
}

let databasePromise: Promise<IDBDatabase> | null = null;

function openDatabase(): Promise<IDBDatabase> {
  if (typeof indexedDB === "undefined") {
    return Promise.reject(new Error("当前浏览器不支持 IndexedDB，无法保存本机密钥"));
  }
  if (databasePromise) return databasePromise;

  databasePromise = new Promise<IDBDatabase>((resolve, reject) => {
    const request = indexedDB.open(DB_NAME, DB_VERSION);
    request.onupgradeneeded = () => {
      const db = request.result;
      if (!db.objectStoreNames.contains(STORE_NAME)) {
        db.createObjectStore(STORE_NAME);
      }
    };
    request.onsuccess = () => resolve(request.result);
    request.onerror = () => reject(request.error ?? new Error("无法打开本地密钥库"));
    request.onblocked = () => reject(new Error("本地密钥库被其他标签页占用，请关闭后重试"));
  });

  return databasePromise;
}

function transactionDone(tx: IDBTransaction): Promise<void> {
  return new Promise((resolve, reject) => {
    tx.oncomplete = () => resolve();
    tx.onerror = () => reject(tx.error ?? new Error("本地密钥库写入失败"));
    tx.onabort = () => reject(tx.error ?? new Error("本地密钥库操作被中止"));
  });
}

export async function loadDeviceKey(): Promise<DeviceKeyRecord | null> {
  const db = await openDatabase();
  return new Promise<DeviceKeyRecord | null>((resolve, reject) => {
    const tx = db.transaction(STORE_NAME, "readonly");
    const request = tx.objectStore(STORE_NAME).get(RECORD_KEY);
    request.onsuccess = () => {
      const value: unknown = request.result;
      if (typeof value !== "object" || value === null) {
        resolve(null);
        return;
      }
      const candidate = value as Partial<DeviceKeyRecord>;
      if (
        !candidate.privateKey ||
        typeof candidate.publicKey !== "string" ||
        (candidate.algorithm !== "ed25519" && candidate.algorithm !== "p256")
      ) {
        resolve(null);
        return;
      }
      resolve(candidate as DeviceKeyRecord);
    };
    request.onerror = () => reject(request.error ?? new Error("读取本机密钥失败"));
  });
}

export async function saveDeviceKey(record: DeviceKeyRecord): Promise<void> {
  const db = await openDatabase();
  const tx = db.transaction(STORE_NAME, "readwrite");
  tx.objectStore(STORE_NAME).put(record, RECORD_KEY);
  await transactionDone(tx);
}

export async function deleteDeviceKey(): Promise<void> {
  const db = await openDatabase();
  const tx = db.transaction(STORE_NAME, "readwrite");
  tx.objectStore(STORE_NAME).delete(RECORD_KEY);
  await transactionDone(tx);
}

export function getClientId(): string | null {
  try {
    const value = localStorage.getItem(CLIENT_ID_KEY);
    return value && value.trim() !== "" ? value : null;
  } catch {
    return null;
  }
}

export function setClientId(clientId: string): void {
  try {
    localStorage.setItem(CLIENT_ID_KEY, clientId);
  } catch {
    // 隐私模式下可能禁止写入；会话仍可工作到本次刷新为止。
  }
}

export function clearClientId(): void {
  try {
    localStorage.removeItem(CLIENT_ID_KEY);
  } catch {
    // 同上。
  }
}

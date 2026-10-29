import type { AuthAlgorithm } from "../api/types";

const AUTH_DOMAIN = "homehubext/auth/v1";

export interface GeneratedDeviceKey {
  /** extractable=false 的 CryptoKey，可被 IndexedDB 结构化克隆保存。 */
  privateKey: CryptoKey;
  /** Go 端期望的 raw 公钥：Ed25519 32 字节 / P-256 未压缩点 65 字节。 */
  publicKey: string;
  algorithm: AuthAlgorithm;
}

function hasWebCrypto(): boolean {
  return typeof crypto !== "undefined" && typeof crypto.subtle !== "undefined";
}

export function webCryptoAvailable(): boolean {
  return hasWebCrypto();
}

function toBase64Url(bytes: Uint8Array): string {
  let binary = "";
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary).replaceAll("+", "-").replaceAll("/", "_").replace(/=+$/u, "");
}

/**
 * 生成客户端密钥对。优先 Ed25519，运行环境不支持时降级为 ECDSA P-256。
 *
 * extractable 必须为 false：私钥字节永远无法导出，只能以 CryptoKey 形式
 * 存放在 IndexedDB 里。这是 XSS 也拿不走长期凭证的前提。
 */
export async function generateDeviceKey(
  preferred: AuthAlgorithm = "ed25519",
): Promise<GeneratedDeviceKey> {
  if (!hasWebCrypto()) {
    throw new Error("当前环境不支持 WebCrypto，请使用 HTTPS 或 localhost 访问");
  }

  let algorithm: AuthAlgorithm = preferred;
  let pair: CryptoKeyPair | null = null;

  if (algorithm === "ed25519") {
    try {
      pair = await crypto.subtle.generateKey({ name: "Ed25519" }, false, ["sign", "verify"]);
    } catch {
      algorithm = "p256";
    }
  }

  if (!pair) {
    pair = await crypto.subtle.generateKey({ name: "ECDSA", namedCurve: "P-256" }, false, [
      "sign",
      "verify",
    ]);
  }

  const raw = await crypto.subtle.exportKey("raw", pair.publicKey);
  return {
    privateKey: pair.privateKey,
    publicKey: toBase64Url(new Uint8Array(raw)),
    algorithm,
  };
}

/** 与服务端逐字节一致，禁止 JSON 序列化。 */
export function authMessage(clientId: string, nonce: string): string {
  return `${AUTH_DOMAIN}\n${clientId}\n${nonce}`;
}

export async function signMessage(
  privateKey: CryptoKey,
  algorithm: AuthAlgorithm,
  message: string,
): Promise<string> {
  const data = new TextEncoder().encode(message);
  const signature =
    algorithm === "ed25519"
      ? await crypto.subtle.sign({ name: "Ed25519" }, privateKey, data)
      : await crypto.subtle.sign({ name: "ECDSA", hash: "SHA-256" }, privateKey, data);
  return toBase64Url(new Uint8Array(signature));
}

// 在没有浏览器的情况下验证 WebCrypto 密钥与签名格式：
// 生成的公钥/签名必须能被 Node 的 WebCrypto 以同样的算法验签，
// 且私钥不可导出、签名编码是无填充的 base64url（Go 端用 RawURLEncoding 解码）。
import assert from "node:assert/strict";
import { fileURLToPath, pathToFileURL } from "node:url";
import { mkdir } from "node:fs/promises";
import { dirname, join, resolve } from "node:path";
import { build } from "vite";

const root = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const outDir = join(root, "node_modules", ".cache", "hh-crypto");
await mkdir(outDir, { recursive: true });

await build({
  configFile: false,
  root,
  logLevel: "silent",
  build: {
    ssr: "src/auth/keys.ts",
    outDir,
    emptyOutDir: true,
    minify: false,
    rollupOptions: {
      output: { entryFileNames: "keys.mjs" },
    },
  },
});

const keys = await import(pathToFileURL(join(outDir, "keys.mjs")).href);

async function verifyCase(algorithm) {
  const generated = await keys.generateDeviceKey(algorithm);
  assert.equal(generated.algorithm, algorithm, "算法必须与请求一致");
  assert.equal(generated.privateKey.extractable, false, "私钥必须不可导出");

  await assert.rejects(
    crypto.subtle.exportKey("pkcs8", generated.privateKey),
    undefined,
    "不可导出的私钥不应能被 exportKey 导出",
  );

  const clientId = "01JC8Z3K9V2Q7X4B6N0M";
  const nonce = "dGVzdC1ub25jZS0zMi1ieXRlcw";
  const message = keys.authMessage(clientId, nonce);
  assert.equal(
    message,
    `homehubext/auth/v1\n${clientId}\n${nonce}`,
    "签名消息必须逐字节符合协议",
  );
  assert.ok(message.includes("\n"), "分隔符必须是真实换行");

  const signature = await keys.signMessage(generated.privateKey, generated.algorithm, message);
  assert.match(signature, /^[A-Za-z0-9_-]+$/, "签名必须是无填充 base64url");
  assert.ok(!signature.includes("="), "签名不能带 padding");

  const publicRaw = Buffer.from(generated.publicKey, "base64url");
  assert.equal(
    publicRaw.length,
    algorithm === "ed25519" ? 32 : 65,
    "公钥必须是 Go 端期望的 raw 字节长度",
  );

  const importAlgorithm =
    algorithm === "ed25519"
      ? { name: "Ed25519" }
      : { name: "ECDSA", namedCurve: "P-256" };
  const verifyAlgorithm =
    algorithm === "ed25519" ? { name: "Ed25519" } : { name: "ECDSA", hash: "SHA-256" };

  const publicKey = await crypto.subtle.importKey("raw", publicRaw, importAlgorithm, false, [
    "verify",
  ]);
  const ok = await crypto.subtle.verify(
    verifyAlgorithm,
    publicKey,
    Buffer.from(signature, "base64url"),
    new TextEncoder().encode(message),
  );
  assert.equal(ok, true, `${algorithm} 签名验签失败`);

  const tampered = new TextEncoder().encode(message.replace(nonce, `${nonce}x`));
  const bad = await crypto.subtle.verify(
    verifyAlgorithm,
    publicKey,
    Buffer.from(signature, "base64url"),
    tampered,
  );
  assert.equal(bad, false, "消息被篡改后签名不应通过");
}

await verifyCase("ed25519");
await verifyCase("p256");
console.log("WebCrypto 自检通过：ed25519 与 p256 的密钥、消息字节与签名编码均正确。");

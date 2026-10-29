import { send } from "./http";
import type {
  AuthAlgorithm,
  AuthSessionResponse,
  ChallengeResponse,
  EnrollResponse,
} from "./types";

export interface EnrollInput {
  token: string;
  name: string;
  algorithm: AuthAlgorithm;
  public_key: string;
}

export function requestChallenge(
  clientId: string,
  signal?: AbortSignal,
): Promise<ChallengeResponse> {
  const init = { method: "POST", body: { client_id: clientId } };
  return send<ChallengeResponse>("/auth/challenge", signal ? { ...init, signal } : init);
}

export interface ExchangeInput {
  client_id: string;
  nonce: string;
  signature: string;
}

export function exchangeToken(input: ExchangeInput, signal?: AbortSignal): Promise<AuthSessionResponse> {
  const init = { method: "POST", body: input };
  return send<AuthSessionResponse>("/auth/token", signal ? { ...init, signal } : init);
}

export function refreshToken(): Promise<AuthSessionResponse> {
  return send<AuthSessionResponse>("/auth/refresh", { method: "POST" });
}

export function enrollDevice(input: EnrollInput, signal?: AbortSignal): Promise<EnrollResponse> {
  const init = { method: "POST", body: input };
  return send<EnrollResponse>("/auth/enroll", signal ? { ...init, signal } : init);
}

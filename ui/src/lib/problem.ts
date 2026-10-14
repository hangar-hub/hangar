import type { Problem } from "@/api/types";

/** An error from the native API, carrying the RFC 9457 problem detail. */
export class ApiError extends Error {
  readonly status: number;
  readonly problem?: Problem;

  constructor(status: number, problem?: Problem, fallback?: string) {
    super(problemText(status, problem, fallback));
    this.name = "ApiError";
    this.status = status;
    this.problem = problem;
  }
}

function problemText(status: number, problem?: Problem, fallback?: string): string {
  if (problem?.detail) return problem.detail;
  if (problem?.errors?.length) return problem.errors.map((e) => e.detail).join(" ");
  if (fallback) return fallback;
  if (problem?.title && problem.title !== "about:blank") return `${problem.title}.`;
  return statusText(status);
}

function statusText(status: number): string {
  switch (status) {
    case 0:
      return "Hangar could not be reached. Check your connection and try again.";
    case 401:
      return "Sign in to continue.";
    case 403:
      return "You don't have permission to do this.";
    case 404:
      return "Not found.";
    case 409:
      return "Someone else changed this at the same time. Reload and try again.";
    case 413:
      return "The request is too large.";
    case 429:
      return "Too many requests. Wait a moment and try again.";
    default:
      return status >= 500
        ? "Hangar had a problem answering. Try again, and ask an admin to check the logs if it keeps happening."
        : `The request failed with status ${status}.`;
  }
}

/** Converts anything thrown into a message for the UI. */
export function errorMessage(err: unknown): string {
  if (err instanceof ApiError) return err.message;
  if (err instanceof Error && err.message) return err.message;
  return "Something went wrong. Try again.";
}

/** The HTTP status of an error, or 0 when it is not an API error. */
export function errorStatus(err: unknown): number {
  return err instanceof ApiError ? err.status : 0;
}

/** Field errors keyed by JSON pointer or parameter name, for forms. */
export function fieldErrors(err: unknown): Record<string, string> {
  const out: Record<string, string> = {};
  if (!(err instanceof ApiError)) return out;
  for (const e of err.problem?.errors ?? []) {
    const key = (e.pointer ?? e.parameter ?? "").replace(/^\//, "");
    if (key) out[key] = e.detail;
  }
  return out;
}

function isProblem(value: unknown): value is Problem {
  return typeof value === "object" && value !== null && "status" in value;
}

/**
 * Unwraps an openapi-fetch result: returns the data, or throws an ApiError
 * built from the problem detail.
 */
export function unwrap<T>(result: { data?: T; error?: unknown; response: Response }): T {
  if (result.error !== undefined || !result.response.ok) {
    const problem = isProblem(result.error) ? result.error : undefined;
    throw new ApiError(result.response.status, problem);
  }
  return result.data as T;
}

/** Like unwrap, for calls that answer 204 with no body. */
export function unwrapEmpty(result: { error?: unknown; response: Response }): void {
  if (result.error !== undefined || !result.response.ok) {
    const problem = isProblem(result.error) ? result.error : undefined;
    throw new ApiError(result.response.status, problem);
  }
}

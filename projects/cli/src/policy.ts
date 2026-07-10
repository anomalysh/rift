// Builds the visitor-access policy the agent declares in its Hello (A2-A5) from
// the parsed flags. The wire shape mirrors the server's core.Policy JSON tags
// exactly; enforcement is server-side. Basic-auth passwords are bcrypt-hashed
// here so the plaintext never leaves this process.

import type { FlagConfig } from "./args.ts";

/** The policy as it travels in the Hello (matches server core.Policy). */
export interface WirePolicy {
  basic_auth?: { user: string; hash: string }[];
  allow_ips?: string[];
  deny_ips?: string[];
  rate_limit?: { rps: number; burst?: number; per_ip?: boolean };
  ttl_seconds?: number;
  once?: boolean;
  max_requests?: number;
}

export type PolicyResult = { policy?: WirePolicy } | { error: string };

// parseDuration turns "30m" / "1h" / "90s" / "120" (bare = seconds) into whole
// seconds, or null if malformed.
function parseDurationSeconds(raw: string): number | null {
  const m = /^(\d+)(s|m|h)?$/.exec(raw.trim());
  if (!m) {
    return null;
  }
  const n = Number.parseInt(m[1] as string, 10);
  switch (m[2]) {
    case "h":
      return n * 3600;
    case "m":
      return n * 60;
    default:
      return n; // "s" or bare
  }
}

// parseRate turns "20/s" or "20" into a requests-per-second number, or null.
function parseRate(raw: string): number | null {
  const m = /^(\d+(?:\.\d+)?)(?:\/s)?$/.exec(raw.trim());
  if (!m) {
    return null;
  }
  const rps = Number.parseFloat(m[1] as string);
  return rps > 0 ? rps : null;
}

/**
 * Build the wire policy from flags, hashing basic-auth passwords. Returns
 * `{ policy }` (undefined when no policy flags were given), or `{ error }` on a
 * malformed value. Async because bcrypt hashing is.
 */
export async function buildPolicy(flags: FlagConfig): Promise<PolicyResult> {
  const p: WirePolicy = {};
  let any = false;

  if (flags.basicAuth && flags.basicAuth.length > 0) {
    const creds: { user: string; hash: string }[] = [];
    for (const entry of flags.basicAuth) {
      const idx = entry.indexOf(":");
      if (idx <= 0 || idx === entry.length - 1) {
        return {
          error: `invalid --basic-auth ${JSON.stringify(entry)}: expected user:password`,
        };
      }
      const user = entry.slice(0, idx);
      const password = entry.slice(idx + 1);
      // Bun ships a native bcrypt; the server verifies with golang.org/x/crypto.
      const hash = await Bun.password.hash(password, "bcrypt");
      creds.push({ user, hash });
    }
    p.basic_auth = creds;
    any = true;
  }

  if (flags.allowIp && flags.allowIp.length > 0) {
    p.allow_ips = flags.allowIp;
    any = true;
  }
  if (flags.denyIp && flags.denyIp.length > 0) {
    p.deny_ips = flags.denyIp;
    any = true;
  }

  if (flags.ttl !== undefined) {
    const secs = parseDurationSeconds(flags.ttl);
    if (secs === null || secs <= 0) {
      return {
        error: `invalid --ttl ${JSON.stringify(flags.ttl)}: expected e.g. 30m, 1h, 90s`,
      };
    }
    p.ttl_seconds = secs;
    any = true;
  }

  if (flags.once) {
    p.once = true;
    any = true;
  }

  if (flags.maxRequests !== undefined) {
    if (!/^\d+$/.test(flags.maxRequests)) {
      return {
        error: `invalid --max-requests ${JSON.stringify(flags.maxRequests)}: expected a whole number`,
      };
    }
    const n = Number.parseInt(flags.maxRequests, 10);
    if (n <= 0) {
      return { error: "--max-requests must be greater than 0" };
    }
    p.max_requests = n;
    any = true;
  }

  if (flags.rateLimit !== undefined) {
    const rps = parseRate(flags.rateLimit);
    if (rps === null) {
      return {
        error: `invalid --rate-limit ${JSON.stringify(flags.rateLimit)}: expected e.g. 20/s`,
      };
    }
    p.rate_limit = { rps };
    any = true;
  }

  return any ? { policy: p } : {};
}

// Decorrelated-jitter reconnection backoff.
//
// Each delay is drawn uniformly from [base, prev * 3], clamped to cap. Compared
// with the alternatives:
//   - full jitter  (uniform in [0, ceiling)) repeatedly collapses back toward
//     zero, so successive delays jump around (…489, 424, 671, 956, 7957…)
//     instead of ramping;
//   - plain exponential has no jitter, so a fleet reconnects in lockstep.
// Decorrelated jitter keeps the randomness that spreads load while letting the
// delay climb smoothly toward the cap and stay there. See AWS's "Exponential
// Backoff And Jitter". The counter is reset once a connection succeeds so a
// brief blip does not leave the next reconnect starting near the cap.

export interface BackoffOptions {
  readonly baseMs: number;
  readonly capMs: number;
  /** Injectable RNG in [0, 1); defaults to Math.random. Present for tests. */
  readonly rand?: () => number;
}

/** Stateful decorrelated-jitter backoff. */
export class Backoff {
  private readonly baseMs: number;
  private readonly capMs: number;
  private readonly rand: () => number;
  private prev = 0;

  constructor(opts: BackoffOptions) {
    this.baseMs = opts.baseMs;
    this.capMs = opts.capMs;
    this.rand = opts.rand ?? Math.random;
  }

  /** Next delay in ms. Monotone in expectation, capped at capMs. */
  next(): number {
    const prev = this.prev > 0 ? this.prev : this.baseMs;
    const upper = Math.min(this.capMs, prev * 3);
    const delay = this.baseMs + this.rand() * (upper - this.baseMs);
    this.prev = Math.min(this.capMs, Math.floor(delay));
    return this.prev;
  }

  /** Forget the ramp; the next delay starts from base again. */
  reset(): void {
    this.prev = 0;
  }
}

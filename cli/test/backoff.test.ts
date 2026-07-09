import { describe, expect, test } from "bun:test";

import { Backoff } from "../src/backoff.ts";

const BASE = 500;
const CAP = 30_000;

describe("Backoff (decorrelated jitter)", () => {
  test("every delay stays within [base, cap]", () => {
    // A fixed RNG at the extremes exercises both ends of the range.
    for (const r of [0, 0.5, 0.999999]) {
      const b = new Backoff({ baseMs: BASE, capMs: CAP, rand: () => r });
      for (let i = 0; i < 50; i++) {
        const d = b.next();
        expect(d).toBeGreaterThanOrEqual(BASE);
        expect(d).toBeLessThanOrEqual(CAP);
      }
    }
  });

  test("ramps upward and then holds at the cap (rand at max)", () => {
    const b = new Backoff({ baseMs: BASE, capMs: CAP, rand: () => 1 });
    const delays: number[] = [];
    for (let i = 0; i < 12; i++) {
      delays.push(b.next());
    }
    // Monotone non-decreasing: no jumping back toward zero like full jitter.
    for (let i = 1; i < delays.length; i++) {
      expect(delays[i]!).toBeGreaterThanOrEqual(delays[i - 1]!);
    }
    // Reaches and stays at the cap.
    expect(delays[delays.length - 1]).toBe(CAP);
  });

  test("first delay never collapses to near-zero", () => {
    // With full jitter, rand()=0 would yield 0ms. Decorrelated jitter floors
    // every delay at base.
    const b = new Backoff({ baseMs: BASE, capMs: CAP, rand: () => 0 });
    expect(b.next()).toBe(BASE);
    expect(b.next()).toBe(BASE);
  });

  test("reset() returns the ramp to base", () => {
    const b = new Backoff({ baseMs: BASE, capMs: CAP, rand: () => 1 });
    for (let i = 0; i < 10; i++) {
      b.next();
    }
    expect(b.next()).toBe(CAP);
    b.reset();
    // Back to the first-step ceiling of base*3.
    const after = b.next();
    expect(after).toBeLessThanOrEqual(BASE * 3);
    expect(after).toBeGreaterThanOrEqual(BASE);
  });

  test("stays within the decorrelated bound prev*3 across a real RNG", () => {
    const b = new Backoff({ baseMs: BASE, capMs: CAP });
    let prev = BASE;
    for (let i = 0; i < 100; i++) {
      const d = b.next();
      expect(d).toBeGreaterThanOrEqual(BASE);
      expect(d).toBeLessThanOrEqual(Math.min(CAP, prev * 3));
      prev = d;
    }
  });
});

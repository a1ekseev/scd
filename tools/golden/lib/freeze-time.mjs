// Freezes time for reproducible golden references. Import BEFORE any dist module.
// new Date() / Date.now() return GOLDEN_NOW (default 2026-08-15T10:00:00.000Z).
export const FROZEN_NOW = process.env.GOLDEN_NOW ?? "2026-08-15T10:00:00.000Z";

const RealDate = globalThis.Date;
const frozenMs = RealDate.parse(FROZEN_NOW);
if (Number.isNaN(frozenMs)) {
  throw new Error(`GOLDEN_NOW is not a valid date: ${FROZEN_NOW}`);
}

class FrozenDate extends RealDate {
  constructor(...args) {
    if (args.length === 0) {
      super(frozenMs);
    } else {
      super(...args);
    }
  }

  static now() {
    return frozenMs;
  }
}

globalThis.Date = FrozenDate;

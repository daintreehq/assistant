import { Queue } from "../src/queue.js";
import { Db } from "../src/storage/db.js";

function makeQueue(): { queue: Queue; db: Db } {
  const db = new Db(":memory:");
  return { queue: new Queue(db), db };
}

describe("Queue", () => {
  it("dedupes events with the same dedupeKey and digest shows one with count 2", () => {
    const { queue } = makeQueue();

    queue.publish({
      source: "system",
      severity: "info",
      title: "Build failing",
      summary: "first occurrence",
      dedupeKey: "build:main",
    });
    queue.publish({
      source: "system",
      severity: "info",
      title: "Build failing",
      summary: "second occurrence",
      dedupeKey: "build:main",
    });

    const digest = queue.digest();
    expect(digest).toHaveLength(1);
    expect(digest[0].count).toBe(2);
    // Latest summary wins on dedupe bump.
    expect(digest[0].summary).toBe("second occurrence");
  });

  it("format() returns a string containing the title", () => {
    const { queue } = makeQueue();

    queue.publish({
      source: "timer",
      severity: "attention",
      title: "Standup reminder",
      summary: "Daily standup in 5 minutes",
    });

    const out = queue.format(queue.digest());
    expect(typeof out).toBe("string");
    expect(out).toContain("Standup reminder");
  });

  it("format() shows the dedupe count multiplier when count > 1", () => {
    const { queue } = makeQueue();

    queue.publish({
      source: "system",
      severity: "info",
      title: "Repeated",
      summary: "again",
      dedupeKey: "k",
    });
    queue.publish({
      source: "system",
      severity: "info",
      title: "Repeated",
      summary: "again",
      dedupeKey: "k",
    });

    const out = queue.format(queue.digest());
    expect(out).toContain("×2");
  });

  it("format() returns the empty-inbox message when there are no events", () => {
    const { queue } = makeQueue();
    expect(queue.format(queue.digest())).toBe("Inbox is empty.");
  });

  it("drops expired events from the digest", () => {
    const { queue } = makeQueue();

    // Already expired: ttl in the (effectively) past.
    queue.publish({
      source: "system",
      severity: "info",
      title: "Stale ping",
      summary: "should be gone",
      ttlMs: -1000,
    });

    expect(queue.digest()).toHaveLength(0);
    expect(queue.format(queue.digest())).toBe("Inbox is empty.");
  });

  it("keeps non-expired ttl events in the digest", () => {
    const { queue } = makeQueue();

    queue.publish({
      source: "system",
      severity: "info",
      title: "Fresh ping",
      summary: "still valid",
      ttlMs: 60_000,
    });

    const digest = queue.digest();
    expect(digest).toHaveLength(1);
    expect(digest[0].title).toBe("Fresh ping");
    expect(queue.format(digest)).toContain("Fresh ping");
  });
});

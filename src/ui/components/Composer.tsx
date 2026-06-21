import { Fragment, useImperativeHandle, useState, type Ref } from "react";
import { TextAttributes } from "@opentui/core";
import { Divider } from "../primitives.js";
import { glyphs, ui, unicodeOk } from "../theme.js";
import { MultilineInput } from "./MultilineInput.js";
import { ThinkingDot } from "./ThinkingDot.js";
import { paletteEntries } from "../../commandRegistry.js";

/**
 * Slash commands surfaced as a filterable palette — described by intent. Derived
 * from the shared command registry so the palette can't drift from the commands
 * the handlers actually accept (issue #50).
 */
export const COMMAND_SUGGESTIONS: Array<[string, string]> = paletteEntries();

/** Cap on the recallable prompt history kept for ↑/↓ during a session. */
const HISTORY_LIMIT = 200;

/**
 * Imperative handle the controller uses to push text back into the composer when
 * a just-sent message is pulled back with Escape (issue #61). The buffer stays
 * owned by the composer (lifting it would re-render the whole tree on every
 * keystroke), so the one out-of-band write is exposed as a method instead.
 */
export interface ComposerHandle {
  /** Replace the composer buffer (cursor parks at end via MultilineInput). */
  restore(text: string): void;
}

function suggestionsFor(value: string): Array<[string, string]> {
  if (!value.startsWith("/")) return [];
  const q = value.slice(1).toLowerCase();
  return COMMAND_SUGGESTIONS.filter(
    ([cmd, desc]) =>
      cmd.slice(1).startsWith(q) || desc.toLowerCase().includes(q),
  ).slice(0, 5);
}

/**
 * The composer. Two visual layers: the input line (a single `›` — Daintree is
 * already named in the header) and a context line whose right side reflects the
 * session, not a fixed shortcut string. Typing `/` opens a command palette;
 * Tab completes the top match. While busy, the actual stage is shown, not a
 * generic "thinking".
 */
export function Composer({
  busy,
  focus = true,
  stage = "Thinking",
  queueDepth = 0,
  contextHint,
  cancellable,
  attentionPending = false,
  onSubmit,
  onCancel,
  ref,
}: {
  busy: boolean;
  /** When false the input ignores keystrokes (e.g. during a turn or a modal). */
  focus?: boolean;
  /** Live stage label shown while busy (Inspecting, Delegating, Watching…). */
  stage?: string;
  /** User follow-ups queued behind the in-flight turn. When >0 the busy
   *  indicator appends "· N queued" so silently-queued input is visible (#95). */
  queueDepth?: number;
  /** Right-aligned context summary on the second line. */
  contextHint?: string;
  /** Whether the in-flight turn can be aborted; gates the "Esc cancel" hint.
   *  Defaults to `busy` so callers that don't distinguish turn kinds still show it. */
  cancellable?: boolean;
  /** Actionable attention is waiting in the inbox: when no cancellable turn is in
   *  flight, lead the hint row with `^O` so the operator notices the ops view. */
  attentionPending?: boolean;
  onSubmit: (value: string) => boolean | void | Promise<void>;
  /** Abort the in-flight turn — invoked on Escape when the composer is empty and
   *  busy. With text present, Escape clears the buffer instead (no cancel). */
  onCancel?: () => void;
  /** Imperative handle (React 19 ref-as-prop) exposing {@link ComposerHandle} so
   *  the controller can restore a pulled-back message into the buffer. */
  ref?: Ref<ComposerHandle>;
}) {
  const [value, setValue] = useState("");
  // The one out-of-band write into the otherwise composer-owned buffer: restoring
  // a message the user pulled back with Escape before any assistant output (#61).
  useImperativeHandle(ref, () => ({ restore: (text: string) => setValue(text) }), []);
  // Session prompt history (oldest first) for ↑/↓ recall in the input. Lives
  // here because this is where accepted submits are observed.
  const [history, setHistory] = useState<string[]>([]);
  const suggestions = focus && !busy ? suggestionsFor(value) : [];
  // Resolve the ASCII fallback once so the prompt glyph and the busy spinner agree
  // on the same character set (no mixed Unicode/ASCII within one composer line).
  const ascii = !unicodeOk();
  const set = glyphs(ascii);

  // The hint row order adapts to what is most relevant right now, but the set of
  // hints stays stable (no new chrome — just promotion). A cancellable turn in
  // flight leads with Esc so the abort gesture is discoverable exactly when it
  // applies; failing that, pending actionable attention leads with `^O` to pull the
  // eye toward the ops view. Cancel takes precedence over attention. `^O` is emitted
  // exactly once regardless of which branch promotes it.
  const cancelActive = cancellable ?? busy;
  const leadWithOps = attentionPending && !cancelActive;
  const hints: Array<{ key: string; action: string }> = [];
  if (cancelActive) hints.push({ key: "Esc", action: "cancel" });
  if (leadWithOps) hints.push({ key: "^O", action: "inspect ops" });
  hints.push({ key: "/", action: "commands" });
  hints.push({ key: "↑", action: "history" });
  if (!leadWithOps) hints.push({ key: "^O", action: "inspect ops" });

  function submit(text: string) {
    const trimmed = text.trim();
    if (!trimmed) return;
    // The controller returns false synchronously when it rejects the submit (a
    // turn is already in flight). Keep the text so it isn't lost; any other
    // result (void, or a Promise for an accepted turn) means it was taken.
    if (onSubmit(trimmed) === false) return;
    // Record the accepted prompt for recall, collapsing immediate repeats and
    // bounding the buffer so a long session (or pasted prompts) can't grow it
    // without limit.
    setHistory((h) =>
      h[h.length - 1] === trimmed ? h : [...h, trimmed].slice(-HISTORY_LIMIT),
    );
    setValue("");
  }

  return (
    <box flexDirection="column" width="100%">
      {suggestions.length > 0 ? (
        <box flexDirection="column" marginBottom={1} paddingLeft={2}>
          {suggestions.map(([cmd, desc]) => (
            // Truncate: these rows live in the repainting region; the native
            // renderer reflows them cleanly, and truncate keeps a long description
            // on a narrow pane from wrapping.
            <text key={cmd} truncate>
              <span fg={ui.color.info}>{cmd.padEnd(14)}</span>
              <span attributes={TextAttributes.DIM}>{desc}</span>
            </text>
          ))}
        </box>
      ) : null}

      <Divider />

      <box flexDirection="row">
        <box flexGrow={1}>
          <MultilineInput
            value={value}
            focus={focus}
            history={history}
            onChange={setValue}
            onSubmit={submit}
            onCancel={() => {
              // Escape: while busy with an empty composer, abort the in-flight turn;
              // otherwise just clear the buffer (the long-standing cancel-edit gesture).
              // Treat a whitespace-only buffer as empty so a stray space doesn't swallow
              // the cancel gesture.
              if (busy && value.trim() === "") onCancel?.();
              else setValue("");
            }}
            onTab={() => {
              if (suggestions.length > 0) setValue(suggestions[0][0] + " ");
            }}
            prompt={set.active === "◌" ? "› " : "> "}
            placeholder="Ask Daintree to supervise, delegate, or inspect…"
          />
        </box>
      </box>

      {/* A compact busy cue at the input: the PRECISE stage (Analyzing request /
          Generating / Delegating / Integrating results / Waiting for approval /
          Cancelling) + any silently-queued follow-ups (#95). The transcript names the
          same state under DAINTREE during silent gaps; here it stays visible right at
          the prompt even while prose streams or the tree fills (where that line hides),
          so you always know work is in flight and the composer is still live. */}
      {busy ? (
        <box flexDirection="row">
          <ThinkingDot ascii={ascii} />
          <text attributes={TextAttributes.DIM} truncate>
            {" "}
            {stage}
            {queueDepth > 0 ? ` · ${queueDepth} queued` : ""}
          </text>
        </box>
      ) : null}

      {/* Bracket the input top AND bottom so the field reads unmistakably as the
          place text goes — the hints below sit outside the rule. */}
      <Divider />

      <box flexDirection="column">
        {/* Hint row: inlined as <span> runs (a native <text> may not nest <text>,
            so we don't use the block <KeyHint> here — same look, valid tree). */}
        <text truncate>
          {hints.map((h, i) => (
            <Fragment key={h.key}>
              {i > 0 ? (
                <span attributes={TextAttributes.DIM}>{" · "}</span>
              ) : null}
              <span fg={ui.color.info}>{h.key}</span>
              <span attributes={TextAttributes.DIM}> {h.action}</span>
            </Fragment>
          ))}
        </text>
        {/* Truncate so a long context summary (many agents/timers) can't widen this
            row past the live terminal on a narrow pane. */}
        {contextHint ? (
          <text attributes={TextAttributes.DIM} truncate>
            {contextHint}
          </text>
        ) : null}
      </box>
    </box>
  );
}

/**
 * Entry point for the UI gallery: `npm run ui:gallery` (run under Bun). A standalone
 * OpenTUI render of {@link UiGallery} with no runtime dependencies, so the visual
 * surface can be iterated on (and screenshot in dark / light / no-color themes) in
 * isolation. INLINE main-screen, like the real cockpit — never the alternate screen.
 */
import { createCliRenderer } from "@opentui/core";
import { createRoot } from "@opentui/react";
import { UiGallery } from "./UiGallery.js";

const renderer = await createCliRenderer({
  screenMode: "main-screen",
  exitOnCtrlC: true,
  useMouse: false,
});
const root = createRoot(renderer);
const exit = () => {
  try {
    root.unmount();
  } catch {
    /* already gone */
  }
  try {
    renderer.destroy();
  } catch {
    /* already destroyed */
  }
  process.exit(0);
};
root.render(<UiGallery exit={exit} />);

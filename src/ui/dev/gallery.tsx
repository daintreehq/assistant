/**
 * Entry point for the UI gallery: `npm run ui:gallery`. A standalone Ink render
 * of {@link UiGallery} with no runtime dependencies, so the visual surface can be
 * iterated on (and screenshot in dark / light / no-color themes) in isolation.
 */
import { render } from "ink";
import { createElement } from "react";
import { UiGallery } from "./UiGallery.js";

render(createElement(UiGallery), { exitOnCtrlC: true });

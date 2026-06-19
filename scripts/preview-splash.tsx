import React from "react";
import { render } from "ink";
import { StartupSplash } from "../src/ui/components/StartupSplash.js";

// Live preview: npx tsx scripts/preview-splash.tsx
function App() {
  return (
    <StartupSplash
      columns={process.stdout.columns || 60}
      rows={process.stdout.rows || 24}
      onComplete={() => setTimeout(() => process.exit(0), 1000)}
    />
  );
}
render(<App />);

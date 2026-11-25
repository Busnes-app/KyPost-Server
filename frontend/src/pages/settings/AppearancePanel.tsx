import { Appearance } from "../../settings/sections/Appearance";
import { ReadingPane } from "../../settings/sections/ReadingPane";

export function AppearancePanel() {
  return (
    <section className="panel appearance-panel">
      <div className="config-header">
        <h2 className="visually-hidden">Appearance</h2>
        <p>Pick a palette and how it handles light and dark, and where an open message shows. Match system switches with your device’s setting; Light or Dark keeps one. Each choice applies straight away and is saved in this browser only.</p>
      </div>
      <div id="theme"><Appearance /></div>
      <ReadingPane />
    </section>
  );
}

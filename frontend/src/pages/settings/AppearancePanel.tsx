import { Appearance } from "../../settings/sections/Appearance";

export function AppearancePanel() {
  return (
    <section className="panel appearance-panel">
      <div className="config-header">
        <h2 className="visually-hidden">Appearance</h2>
        <p>Pick a palette and how it handles light and dark. Match system switches with your device’s setting; Light or Dark keeps one. Your choice applies straight away and is saved in this browser only.</p>
      </div>
      <div id="theme"><Appearance /></div>
    </section>
  );
}

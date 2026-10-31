import { Appearance } from "../../settings/sections/Appearance";

export function AppearancePanel() {
  return (
    <section className="panel appearance-panel">
      <header className="ky-page-head">
        <div>
          <h1 className="ky-page-title">Appearance</h1>
          <p className="ky-page-lede">Pick a palette and how it handles light and dark. Match system switches with your device’s setting; Light or Dark keeps one. Your choice applies straight away and is saved in this browser only.</p>
        </div>
      </header>
      <div id="theme"><Appearance /></div>
    </section>
  );
}

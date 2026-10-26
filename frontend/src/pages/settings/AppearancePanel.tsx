import { Appearance } from "../../settings/sections/Appearance";

export function AppearancePanel() {
  return (
    <section className="panel">
      <h1>Appearance</h1>
      <p className="ky-appearance-description">Pick a theme. It applies straight away and is saved in this browser only. Match system follows your device’s light or dark setting.</p>
      <div id="theme"><Appearance /></div>
    </section>
  );
}

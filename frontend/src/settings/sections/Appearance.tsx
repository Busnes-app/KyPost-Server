import { useEffect, useState } from "react";
import { SYSTEM_THEME, THEME_CATALOG } from "../../ky-ui/themes";
import { applyTheme, getStoredChoice, type ThemeName } from "../../theme";

const MODE = { system: "Follows device", light: "Light", dark: "Dark" };

/**
 * One native radio card per theme, previewed in that theme's own colours by
 * ky-ui (no inline styles), applied as soon as it is chosen.
 */
export function Appearance() {
  const [choice, setChoice] = useState<ThemeName>(getStoredChoice);
  useEffect(() => {
    const sync = () => setChoice(getStoredChoice());
    window.addEventListener("storage", sync);
    return () => window.removeEventListener("storage", sync);
  }, []);

  function choose(value: ThemeName) {
    setChoice(value);
    applyTheme(value);
  }

  return (
    <fieldset className="ky-appearance">
      <legend className="ky-appearance-legend">Theme</legend>
      <div className="ky-appearance-grid">
        {[SYSTEM_THEME, ...THEME_CATALOG].map((theme) => {
          const value = theme === SYSTEM_THEME ? theme.id : theme.name;
          return (
            <label key={theme.id} className="ky-appearance-option">
              <input className="ky-appearance-radio" type="radio" name="kypost-theme" value={value}
                checked={choice === value} onChange={() => choose(value)} />
              <span className="ky-appearance-preview" data-ky-preview={theme.id} aria-hidden="true" />
              <span className="ky-appearance-name">{theme.name}</span>
              <small className="ky-appearance-mode">{MODE[theme.scheme]}</small>
            </label>
          );
        })}
      </div>
    </fieldset>
  );
}

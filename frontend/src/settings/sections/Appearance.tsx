import { useEffect, useState } from "react";
import { findTheme } from "../../ky-ui/themes";
import { CheckIcon } from "../../pages/read/icons";
import { THEME_CHOICES, applyTheme, getStoredChoice, type ThemeName } from "../../theme";

/**
 * Six palette pairs, each offering Match system, Light and Dark, as one native
 * radio group so arrow keys move through every choice. Previews come from ky-ui
 * (no inline styles); a choice applies as soon as it is made.
 */
export function Appearance() {
  const [choice, setChoice] = useState<ThemeName>(getStoredChoice);
  useEffect(() => {
    const sync = () => setChoice(getStoredChoice());
    window.addEventListener("storage", sync);
    return () => window.removeEventListener("storage", sync);
  }, []);

  return (
    <fieldset className="ky-appearance">
      <legend className="ky-appearance-legend">Theme</legend>
      <div className="ky-appearance-grid appearance-pairs">
        {THEME_CHOICES.map(({ pair, choices }) => (
          <div key={pair.id} role="group" aria-labelledby={`${pair.id}-name`} className="appearance-pair">
            <div className="appearance-pair-head">
              <h2 id={`${pair.id}-name`} className="appearance-pair-name">{pair.name}</h2>
              <span className="appearance-pair-halves">{findTheme(pair.light)?.name} · {findTheme(pair.dark)?.name}</span>
            </div>
            <span className="ky-appearance-preview" data-ky-preview={pair.id} aria-hidden="true" />
            <div className="appearance-modes">
              {choices.map(({ mode, label, value }) => (
                <label key={mode} className="appearance-mode">
                  <input type="radio" name="kypost-theme" value={value} checked={choice === value}
                    onChange={() => { setChoice(value); applyTheme(value); }} />
                  {choice === value ? <CheckIcon /> : null}
                  <span>{label}</span>
                </label>
              ))}
            </div>
          </div>
        ))}
      </div>
    </fieldset>
  );
}
